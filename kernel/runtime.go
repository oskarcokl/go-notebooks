package kernel

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/types"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/traefik/yaegi/interp"

	"go-notebooks/kernel/proto"
)

// Package of helpers visible to generated code.
const (
	rtPath  = "gonotebooks/rt"
	rtAlias = "rt" + mark
)

func parseStmt(src string) ast.Node {
	e, err := parser.ParseExpr("func() {" + src + "\n}")
	if err != nil {
		return nil
	}
	list := e.(*ast.FuncLit).Body.List
	if len(list) != 1 {
		return nil
	}
	return list[0]
}

// runtimeState is the part of the session that running cells talk to.
type runtimeState struct {
	mu      sync.Mutex
	cell    string
	step    int
	result  string
	notices []string
	defers  map[string][]func()
	types   []types.Type
	named   map[string]types.Type // Yaegi's name of a notebook type -> type

	goroutines atomic.Int64
}

func (r *runtimeState) begin(cell string) {
	r.cell, r.step, r.result, r.notices, r.types = cell, 0, "", nil, nil
}

func (r *runtimeState) notice(s string) { r.notices = append(r.notices, s) }

func (r *runtimeState) typeID(t types.Type) int {
	r.types = append(r.types, t)
	return len(r.types) - 1
}

// runDefers runs the pending cell-level defers of a cell, last one first.
func (r *runtimeState) runDefers(cell string) {
	r.mu.Lock()
	list := r.defers[cell]
	delete(r.defers, cell)
	r.mu.Unlock()
	for i := len(list) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if x := recover(); x != nil {
					fmt.Fprintf(os.Stderr, "panic in deferred call: %v\n", x)
				}
			}()
			list[i]()
		}()
	}
}

func (r *runtimeState) exports() interp.Exports {
	return interp.Exports{rtPath + "/rt": {
		"Step": reflect.ValueOf(func(i int) { r.step = i }),
		"Display": reflect.ValueOf(func(id int, v any) {
			r.result = r.formatResult(r.types[id], v)
		}),
		"Defer": reflect.ValueOf(func(f func()) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.defers[r.cell] = append(r.defers[r.cell], f)
		}),
		"GoStart": reflect.ValueOf(func() { r.goroutines.Add(1) }),
		"GoDone":  reflect.ValueOf(func() { r.goroutines.Add(-1) }),
		"GoPanic": reflect.ValueOf(func(v any) {
			if rv, ok := v.(reflect.Value); ok && rv.IsValid() {
				v = rv.Interface()
			}
			fmt.Fprintf(os.Stderr, "panic in goroutine: %v\n", show(v))
		}),
		"Show":    reflect.ValueOf(show),
		"ShowAll": reflect.ValueOf(func(vs []any) []any { return showAll(vs) }),
		"Err":     reflect.ValueOf(func(err error) any { return err }),
	}}
}

// Close runs all pending defers.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cells []string
	for c := range s.rt.defers {
		cells = append(cells, c)
	}
	sort.Strings(cells)
	for _, c := range cells {
		s.rt.runDefers(c)
	}
}

// Yaegi stores a dynamic value of an interpreted type as valueInterface, which
// carries the type next to the value. These helpers look inside it.

const yaegiBox = "interp.valueInterface"

func isBox(v reflect.Value) bool {
	return v.IsValid() && v.Kind() == reflect.Struct && v.Type().String() == yaegiBox
}

// unbox returns the value inside a valueInterface and the name of its
// interpreted type, e.g. "main.User" or "*main.User".
func unbox(v reflect.Value) (inner reflect.Value, typ string, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	p := reflect.New(v.Type()).Elem()
	p.Set(v)
	vf := p.FieldByName("value")
	if vf.Type() != reflect.TypeOf(reflect.Value{}) {
		return inner, "", false
	}
	inner = *(*reflect.Value)(unsafe.Pointer(vf.UnsafeAddr()))
	if n := p.FieldByName("node"); n.IsValid() && !n.IsNil() {
		if t := n.Elem().FieldByName("typ"); t.IsValid() && !t.IsNil() {
			if str := t.Elem().FieldByName("str"); str.IsValid() {
				typ = str.String()
			}
		}
	}
	return inner, typ, true
}

// show returns v without Yaegi's wrappers, copying []any and map[string]any
// that contain them.
func show(v any) any {
	rv := reflect.ValueOf(v)
	switch {
	case !rv.IsValid():
		return v
	case isBox(rv):

		if in, _, ok := unbox(rv); ok {
			if !in.IsValid() {
				return nil
			}
			if in.CanInterface() {
				return show(in.Interface())
			}
		}
	case rv.Kind() == reflect.Slice && hasBoxElem(rv.Type()) && !rv.IsNil():
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = show(rv.Index(i).Interface())
		}
		return out
	case rv.Kind() == reflect.Map && hasBoxElem(rv.Type()) && !rv.IsNil():
		out := reflect.MakeMap(reflect.MapOf(rv.Type().Key(), reflect.TypeFor[any]()))
		for it := rv.MapRange(); it.Next(); {
			x := reflect.New(reflect.TypeFor[any]()).Elem()
			if e := show(it.Value().Interface()); e != nil {
				x.Set(reflect.ValueOf(e))
			}
			out.SetMapIndex(it.Key(), x)
		}
		return out.Interface()
	}
	return v
}

func hasBoxElem(t reflect.Type) bool {
	e := t.Elem()
	return e.Kind() == reflect.Interface && e.NumMethod() == 0 || e.String() == yaegiBox
}

func showAll(vs []any) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = show(v)
	}
	return out
}

func (r *runtimeState) formatResult(t types.Type, v any) string {
	if v == nil {
		if _, ok := t.Underlying().(*types.Interface); ok && implementsError(t) {
			return ""
		}
		return "nil"
	}
	if str, ok := v.(string); ok {
		if b, isBasic := t.Underlying().(*types.Basic); !isBasic || b.Info()&types.IsString == 0 {
			return str // String() or Error() of t
		}
	}
	f := formatter{max: 4000, depth: 8, maps: true, named: r.named}
	return f.format(reflect.ValueOf(v), t)
}

// Variables lists the live variables, and Types the tracked types.
func (s *Session) Variables(cells map[string]int) proto.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cells = cells
	globals := s.in.Globals()
	resp := proto.Response{Variables: []proto.Variable{}, Types: []proto.Type{}}
	for _, v := range s.sortedVars() {
		out := proto.Variable{Name: v.name, Type: s.displayType(v.typ), CellID: v.cell, Stale: v.stale}
		for n, ver := range v.versions {
			out.Versions = append(out.Versions, fmt.Sprintf("%s v%d", n, ver))
		}
		sort.Strings(out.Versions)
		if rv, ok := globals[v.internal]; ok {
			f := formatter{max: 300, depth: 4, maps: !s.rt.busy(), named: s.rt.named}
			out.Value = f.format(rv, v.typ)
		}
		if v.stale {
			st := s.staleVar(v)
			out.Reason, out.Changes = st.Reason, st.Changes
		}
		resp.Variables = append(resp.Variables, out)
	}
	for _, ti := range s.types {
		last := ti.versions[len(ti.versions)-1]
		resp.Types = append(resp.Types, proto.Type{Name: ti.name, Version: ti.version(), CellID: ti.cell,
			Schema: last.text, Fields: last.fields, History: ti.history})
	}
	sort.Slice(resp.Types, func(i, j int) bool { return resp.Types[i].Name < resp.Types[j].Name })
	return resp
}

// scanDynamic marks variables stale whose static type did not change but that
// hold, in an interface, a value of an outdated type.
func (s *Session) scanDynamic() {
	old := map[string]string{} // "main.User" -> type name
	for _, ti := range s.types {
		for i, sc := range ti.versions {
			if i < len(ti.versions)-1 && sc.internal != ti.internal {
				old["main."+sc.internal] = ti.name
			}
		}
	}
	if len(old) == 0 || s.rt.busy() {
		return
	}
	globals := s.in.Globals()
	for _, v := range s.vars {
		rv, ok := globals[v.internal]
		if v.stale || !ok {
			continue
		}
		budget := 10000
		found := map[string]int{}
		s.findBoxes(rv, old, found, &budget, map[unsafe.Pointer]bool{})
		for n, ver := range found {
			v.stale = true
			v.versions[n] = ver
		}
	}
}

func (s *Session) findBoxes(v reflect.Value, old map[string]string, found map[string]int, budget *int, seen map[unsafe.Pointer]bool) {
	if *budget--; *budget < 0 || !v.IsValid() {
		return
	}
	rec := func(v reflect.Value) { s.findBoxes(v, old, found, budget, seen) }
	switch v.Kind() {
	case reflect.Interface:
		rec(v.Elem())
	case reflect.Pointer:
		if v.IsNil() || seen[v.UnsafePointer()] {
			return
		}
		seen[v.UnsafePointer()] = true
		// Compiled types cannot hold interpreted values worth chasing.
		if v.Type().Elem().PkgPath() == "" || isBox(v.Elem()) {
			rec(v.Elem())
		}
	case reflect.Slice, reflect.Array:
		switch v.Type().Elem().Kind() {
		case reflect.Interface, reflect.Struct, reflect.Pointer, reflect.Slice, reflect.Map:
			for i := 0; i < v.Len() && *budget > 0; i++ {
				rec(v.Index(i))
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next() && *budget > 0; {
			rec(it.Value())
		}
	case reflect.Struct:
		if isBox(v) {
			if !v.CanInterface() {
				return
			}
			in, typ, ok := unbox(v)
			if !ok {
				return
			}
			internal := strings.TrimLeft(typ, "*[]")
			if n, isOld := old[internal]; isOld {
				for i, sc := range s.types[n].versions {
					if "main."+sc.internal == internal {
						found[n] = i + 1
					}
				}
			}
			rec(in)
			return
		}
		if v.Type().PkgPath() == "" {
			for i := 0; i < v.NumField(); i++ {
				rec(v.Field(i))
			}
		}
	}
}
