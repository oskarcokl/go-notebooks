package builder

import (
	"fmt"
	"go/constant"
	"go/types"
	"math/big"
	"sort"
	"strings"
)

// extract generates a Go file that registers the exported symbols of pkg in
// the form Yaegi expects (what "yaegi extract" produces). It works from
// compiler export data, so cgo packages need no special treatment. Symbols for
// which skip returns true are left out. It returns "" if nothing is left.
func extract(pkg *types.Package, skip func(name string) bool) string {
	x := &extractor{pkg: pkg, imports: map[string]string{}}
	self := x.alias(pkg.Path())
	var entries, wrappers []string
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}
		q := self + "." + name
		val := ""
		if skip(name) {
			continue
		}
		switch o := obj.(type) {
		case *types.Const:
			if b, ok := o.Type().(*types.Basic); ok && b.Info()&types.IsUntyped != 0 {
				val = x.constant(q, o.Val())
			} else {
				val = "reflect.ValueOf(" + q + ")"
			}
		case *types.Func:
			if o.Type().(*types.Signature).TypeParams().Len() == 0 {
				val = "reflect.ValueOf(" + q + ")"
			}
		case *types.Var:
			val = "reflect.ValueOf(&" + q + ").Elem()"
		case *types.TypeName:
			if generic(o.Type()) {
				break
			}
			iface, isIface := o.Type().Underlying().(*types.Interface)
			if isIface && !iface.IsMethodSet() {
				break // a constraint
			}
			val = "reflect.ValueOf((*" + q + ")(nil))"
			if isIface {
				if decl := x.wrapper(name, iface); decl != "" {
					wrappers = append(wrappers, decl)
					entries = append(entries, fmt.Sprintf("%q: reflect.ValueOf((*%s)(nil)),", "_"+name, x.wrapperName(name)))
				}
			}
		}
		if val != "" {
			entries = append(entries, fmt.Sprintf("%q: %s,", name, val))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t\"reflect\"\n")
	if x.usesConst {
		b.WriteString("\t\"go/constant\"\n\t\"go/token\"\n")
	}
	var paths []string
	for p := range x.imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&b, "\t%s %q\n", x.imports[p], p)
	}
	b.WriteString(")\n\n")
	body := strings.Join(entries, "\n\t\t") + strings.Join(wrappers, "\n")
	if !strings.Contains(body, self+".") {
		fmt.Fprintf(&b, "var _ = %s\n", firstRef(self, pkg))
	}
	fmt.Fprintf(&b, "func init() {\n\tsymbols[%q] = map[string]reflect.Value{\n\t\t%s\n\t}\n}\n\n%s\n",
		pkg.Path()+"/"+pkg.Name(), strings.Join(entries, "\n\t\t"), strings.Join(wrappers, "\n"))
	return b.String()
}

func firstRef(self string, pkg *types.Package) string { return self + "." + pkg.Scope().Names()[0] }

func generic(t types.Type) bool {
	switch t := t.(type) {
	case *types.Named:
		return t.TypeParams().Len() > 0
	case *types.Alias:
		return t.TypeParams().Len() > 0
	}
	return false
}

type extractor struct {
	pkg       *types.Package
	imports   map[string]string
	usesConst bool
}

func (x *extractor) alias(path string) string {
	if a, ok := x.imports[path]; ok {
		return a
	}
	a := fmt.Sprintf("p%d", len(x.imports))
	x.imports[path] = a
	return a
}

func (x *extractor) constant(q string, v constant.Value) string {
	var tok, str string
	switch v.Kind() {
	case constant.String:
		tok, str = "STRING", v.ExactString()
	case constant.Int:
		tok, str = "INT", v.ExactString()
	case constant.Float:
		f, ok := constant.Val(v).(*big.Float)
		if !ok {
			f = new(big.Float).SetRat(constant.Val(v).(*big.Rat))
		}
		tok, str = "FLOAT", f.Text('g', int(f.Prec()))
	default:
		return "reflect.ValueOf(" + q + ")"
	}
	x.usesConst = true
	return fmt.Sprintf("reflect.ValueOf(constant.MakeFromLiteral(%q, token.%s, 0))", str, tok)
}

func (x *extractor) wrapperName(name string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '/' || r == '.' || r == '-' {
			return '_'
		}
		return r
	}, x.pkg.Path())
	return "_" + clean + "_" + name
}

// wrapper declares the struct by which interpreted values implement a compiled
// interface. It returns "" if the interface cannot be implemented outside its
// package or mentions types that cannot be named here.
func (x *extractor) wrapper(name string, iface *types.Interface) (decl string) {
	saved := map[string]string{}
	for k, v := range x.imports {
		saved[k] = v
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(unnameable); !ok {
				panic(r)
			}
			decl = ""
			x.imports = saved
		}
	}()
	qual := func(p *types.Package) string {
		if strings.Contains("/"+p.Path()+"/", "/internal/") || strings.HasPrefix(p.Path(), "vendor/") {
			panic(unnameable{})
		}
		return x.alias(p.Path())
	}
	var fields, methods strings.Builder
	w := x.wrapperName(name)
	for i := 0; i < iface.NumMethods(); i++ {
		m := iface.Method(i)
		if !m.Exported() {
			return ""
		}
		sig := m.Type().(*types.Signature)
		checkNameable(sig, map[types.Type]bool{})
		var params, args []string
		for j := 0; j < sig.Params().Len(); j++ {
			t := sig.Params().At(j).Type()
			a, ts := fmt.Sprintf("a%d", j), types.TypeString(t, qual)
			if sig.Variadic() && j == sig.Params().Len()-1 {
				ts = "..." + types.TypeString(t.(*types.Slice).Elem(), qual)
				args = append(args, a+"...")
			} else {
				args = append(args, a)
			}
			params = append(params, a+" "+ts)
		}
		var results []string
		for j := 0; j < sig.Results().Len(); j++ {
			results = append(results, types.TypeString(sig.Results().At(j).Type(), qual))
		}
		ps, rs, ret := strings.Join(params, ", "), strings.Join(results, ", "), ""
		if len(results) > 0 {
			ret = "return "
		}
		fmt.Fprintf(&fields, "\tW%s func(%s) (%s)\n", m.Name(), ps, rs)
		guard := ""
		if m.Name() == "String" && ps == "" && rs == "string" {
			guard = "if W.WString == nil { return \"\" }; "
		}
		fmt.Fprintf(&methods, "func (W %s) %s(%s) (%s) { %s%sW.W%s(%s) }\n", w, m.Name(), ps, rs, guard, ret, m.Name(), strings.Join(args, ", "))
	}
	return fmt.Sprintf("type %s struct {\n\tIValue interface{}\n%s}\n%s", w, fields.String(), methods.String())
}

type unnameable struct{}

// checkNameable panics if t mentions an unexported type of another package.
func checkNameable(t types.Type, seen map[types.Type]bool) {
	if seen[t] {
		return
	}
	seen[t] = true
	rec := func(t types.Type) { checkNameable(t, seen) }
	switch t := t.(type) {
	case *types.Named:
		if t.Obj().Pkg() != nil && !t.Obj().Exported() {
			panic(unnameable{})
		}
		for i := 0; i < t.TypeArgs().Len(); i++ {
			rec(t.TypeArgs().At(i))
		}
	case *types.Alias:
		if t.Obj().Pkg() != nil && !t.Obj().Exported() {
			panic(unnameable{})
		}
	case *types.Pointer:
		rec(t.Elem())
	case *types.Slice:
		rec(t.Elem())
	case *types.Array:
		rec(t.Elem())
	case *types.Chan:
		rec(t.Elem())
	case *types.Map:
		rec(t.Key())
		rec(t.Elem())
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if !t.Field(i).Exported() {
				panic(unnameable{})
			}
			rec(t.Field(i).Type())
		}
	case *types.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			if !t.Method(i).Exported() {
				panic(unnameable{})
			}
			rec(t.Method(i).Type())
		}
	case *types.Signature:
		for i := 0; i < t.Params().Len(); i++ {
			rec(t.Params().At(i).Type())
		}
		for i := 0; i < t.Results().Len(); i++ {
			rec(t.Results().At(i).Type())
		}
	}
}
