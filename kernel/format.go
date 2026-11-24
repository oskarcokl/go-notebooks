package kernel

import (
	"fmt"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// busy reports whether goroutines started by cells are running; maps they
// write to must not be iterated meanwhile.
func (r *runtimeState) busy() bool { return r.goroutines.Load() > 0 }

// formatter renders values with the names of their notebook types, which the
// interpreter's reflect types lack.
type formatter struct {
	max   int
	depth int
	maps  bool
	named map[string]types.Type
	b     strings.Builder
}

func (f *formatter) format(v reflect.Value, t types.Type) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("<%v>", r)
		}
	}()
	f.write(v, t, f.depth)
	out = f.b.String()
	if len(out) > f.max {
		cut := f.max
		for cut > 0 && out[cut]&0xC0 == 0x80 {
			cut--
		}
		out = out[:cut] + "…"
	}
	return out
}

func short(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p.Path() == mainPath {
			return ""
		}
		return p.Name()
	})
}

func (f *formatter) write(v reflect.Value, t types.Type, depth int) {
	w := f.b.WriteString
	if f.b.Len() > f.max {
		return
	}
	for v.IsValid() && (v.Kind() == reflect.Interface || isBox(v)) {
		if isBox(v) {
			in, name, ok := unbox(v)
			if !ok {
				break
			}
			v, t = in, f.named[strings.TrimPrefix(name, "*")]
			if t != nil && name[0] == '*' {
				t = types.NewPointer(t)
			}
			continue
		}
		v = v.Elem()
		if t != nil {
			if _, ok := t.Underlying().(*types.Interface); ok {
				t = nil
			}
		}
	}
	if !v.IsValid() {
		w("nil")
		return
	}
	// Values of compiled types know best how to print themselves.
	if v.CanInterface() && v.Kind() != reflect.Func {
		switch x := v.Interface().(type) {
		case error:
			if !nilable(v) {
				w(x.Error())
				return
			}
		case fmt.Stringer:
			if !nilable(v) {
				w(x.String())
				return
			}
		}
	}
	if t == nil && v.Kind() == reflect.Struct {
		t = f.guess(v.Type())
	}
	var under types.Type
	if t != nil {
		under = t.Underlying()
	}
	elem := func() types.Type {
		switch u := under.(type) {
		case *types.Slice:
			return u.Elem()
		case *types.Array:
			return u.Elem()
		case *types.Map:
			return u.Elem()
		case *types.Pointer:
			return u.Elem()
		}
		return nil
	}
	name := func() string {
		if t != nil {
			return short(t)
		}
		return strings.ReplaceAll(v.Type().String(), "interface {}", "any")
	}
	switch v.Kind() {
	case reflect.String:
		w(strconv.Quote(v.String()))
	case reflect.Pointer:
		if v.IsNil() {
			w("nil")
		} else if v.Type().PkgPath() == "" && v.Elem().Kind() == reflect.Struct && (t == nil || isMainPtr(t)) && depth > 0 {
			w("&")
			f.write(v.Elem(), elem(), depth-1)
		} else if v.Elem().Kind() != reflect.Struct && depth > 0 {
			w("&")
			f.write(v.Elem(), elem(), depth-1)
		} else {
			w("(" + name() + ")(" + fmt.Sprintf("%#x", v.Pointer()) + ")")
		}
	case reflect.Struct:
		n := ""
		if _, isStruct := t.(*types.Struct); t != nil && !isStruct {
			n = short(t)
		} else if t == nil && v.Type().Name() != "" {
			n = v.Type().String()
		}
		w(n + "{")
		if depth <= 0 {
			w("…}")
			return
		}
		st, _ := under.(*types.Struct)
		for i := 0; i < v.NumField(); i++ {
			if i > 0 {
				w(", ")
			}
			fn, ft := v.Type().Field(i).Name, types.Type(nil)
			if st != nil && i < st.NumFields() {
				fn, ft = st.Field(i).Name(), st.Field(i).Type()
			}
			w(fn + ": ")
			f.write(v.Field(i), ft, depth-1)
		}
		w("}")
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			w(name() + "(nil)")
			return
		}
		if v.Type().Elem().Kind() == reflect.Uint8 && v.Kind() == reflect.Slice {
			if b := v.Bytes(); len(b) > 64 {
				w(fmt.Sprintf("[]byte (%d bytes) %q…", len(b), b[:64]))
			} else {
				w(fmt.Sprintf("[]byte(%q)", b))
			}
			return
		}
		w(name() + "{")
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				w(", ")
			}
			if depth <= 0 || f.b.Len() > f.max || i >= 100 {
				w(fmt.Sprintf("… %d more", v.Len()-i))
				break
			}
			f.write(v.Index(i), elem(), depth-1)
		}
		w("}")
	case reflect.Map:
		if v.IsNil() {
			w(name() + "(nil)")
			return
		}
		if !f.maps || depth <= 0 {
			w(fmt.Sprintf("%s (len %d)", name(), v.Len()))
			return
		}
		var kt types.Type
		if m, ok := under.(*types.Map); ok {
			kt = m.Key()
		}
		type kv struct{ k, v string }
		var kvs []kv
		for it := v.MapRange(); it.Next() && len(kvs) < 100; {
			a := formatter{max: f.max, depth: depth - 1, maps: f.maps, named: f.named}
			b := formatter{max: f.max, depth: depth - 1, maps: f.maps, named: f.named}
			kvs = append(kvs, kv{a.format(it.Key(), kt), b.format(it.Value(), elem())})
		}
		sort.Slice(kvs, func(i, j int) bool {
			if len(kvs[i].k) != len(kvs[j].k) && kvs[i].k != "" && kvs[i].k[0] != '"' {
				return len(kvs[i].k) < len(kvs[j].k)
			}
			return kvs[i].k < kvs[j].k
		})
		w(name() + "{")
		for i, e := range kvs {
			if i > 0 {
				w(", ")
			}
			w(e.k + ": " + e.v)
		}
		if v.Len() > len(kvs) {
			w(fmt.Sprintf(", … %d more", v.Len()-len(kvs)))
		}
		w("}")
	case reflect.Func:
		if v.IsNil() {
			w("nil")
		} else {
			w(name())
		}
	case reflect.Chan:
		if v.IsNil() {
			w("nil")
		} else {
			w(fmt.Sprintf("%s (%d/%d buffered)", name(), v.Len(), v.Cap()))
		}
	default:
		w(fmt.Sprintf("%v", v))
	}
}

// guess finds the notebook type a bare struct was made from, if its fields
// identify it. Yaegi does not always keep the type of dynamic values.
func (f *formatter) guess(rt reflect.Type) types.Type {
	var found types.Type
	if rt.Name() != "" || rt.NumField() == 0 {
		return nil
	}
	for _, t := range f.named {
		st, ok := t.Underlying().(*types.Struct)
		if !ok || st.NumFields() != rt.NumField() {
			continue
		}
		same := true
		for i := 0; i < st.NumFields() && same; i++ {
			n := st.Field(i).Name()
			if !st.Field(i).Exported() {
				n = "X" + n
			}
			same = n == rt.Field(i).Name
		}
		if same {
			if found != nil && short(found) != short(t) {
				return nil
			}
			found = t
		}
	}
	return found
}

func nilable(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// isMainPtr reports whether t points to a notebook type.
func isMainPtr(t types.Type) bool {
	p, ok := t.Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	if _, anon := p.Elem().(*types.Struct); anon {
		return true
	}
	return mainTypeName(p.Elem()) != nil
}
