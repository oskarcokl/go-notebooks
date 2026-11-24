package kernel

import (
	"fmt"
	"go/types"
	"strings"
)

// typeNamer says how to spell a named type.
type typeNamer func(*types.TypeName) string

// typeString renders t as Go source. It fails for types that cannot be spelled
// outside their package.
func typeString(t types.Type, name typeNamer) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(unspeakable)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("type %s is not exported by its package", string(e))
		}
	}()
	var b strings.Builder
	writeType(&b, t, name)
	return b.String(), nil
}

type unspeakable string

func writeType(b *strings.Builder, t types.Type, name typeNamer) {
	w := func(s string) { b.WriteString(s) }
	rec := func(t types.Type) { writeType(b, t, name) }
	named := func(obj *types.TypeName, args *types.TypeList) {
		if obj.Pkg() != nil && obj.Pkg().Path() != mainPath && !obj.Exported() {
			panic(unspeakable(obj.Pkg().Name() + "." + obj.Name()))
		}
		w(name(obj))
		if args.Len() > 0 {
			w("[")
			for i := 0; i < args.Len(); i++ {
				if i > 0 {
					w(", ")
				}
				rec(args.At(i))
			}
			w("]")
		}
	}
	switch t := t.(type) {
	case *types.Named:
		named(t.Obj(), t.TypeArgs())
	case *types.Alias:
		if o := t.Obj(); o.Pkg() != nil && o.Pkg().Path() != mainPath && !o.Exported() {
			rec(types.Unalias(t))
			return
		}
		named(t.Obj(), t.TypeArgs())
	case *types.TypeParam:
		w(t.Obj().Name())
	case *types.Pointer:
		w("*")
		rec(t.Elem())
	case *types.Slice:
		w("[]")
		rec(t.Elem())
	case *types.Array:
		w(fmt.Sprintf("[%d]", t.Len()))
		rec(t.Elem())
	case *types.Map:
		w("map[")
		rec(t.Key())
		w("]")
		rec(t.Elem())
	case *types.Chan:
		switch t.Dir() {
		case types.SendRecv:
			w("chan ")
		case types.SendOnly:
			w("chan<- ")
		case types.RecvOnly:
			w("<-chan ")
		}
		if c, ok := t.Elem().(*types.Chan); ok && c.Dir() == types.RecvOnly {
			w("(")
			rec(t.Elem())
			w(")")
			return
		}
		rec(t.Elem())
	case *types.Struct:
		w("struct{")
		for i := 0; i < t.NumFields(); i++ {
			if i > 0 {
				w("; ")
			}
			f := t.Field(i)
			if !f.Embedded() {
				w(f.Name() + " ")
			}
			rec(f.Type())
			if tag := t.Tag(i); tag != "" {
				w(fmt.Sprintf(" %q", tag))
			}
		}
		w("}")
	case *types.Signature:
		w("func")
		writeSignature(b, t, name)
	case *types.Interface:
		if t.Empty() {
			w("interface{}")
			return
		}
		w("interface{")
		for i := 0; i < t.NumExplicitMethods(); i++ {
			if i > 0 {
				w("; ")
			}
			m := t.ExplicitMethod(i)
			w(m.Name())
			writeSignature(b, m.Type().(*types.Signature), name)
		}
		for i := 0; i < t.NumEmbeddeds(); i++ {
			if i > 0 || t.NumExplicitMethods() > 0 {
				w("; ")
			}
			rec(t.EmbeddedType(i))
		}
		w("}")
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			if i > 0 {
				w(" | ")
			}
			if t.Term(i).Tilde() {
				w("~")
			}
			rec(t.Term(i).Type())
		}
	default:
		w(t.String())
	}
}

// writeSignature writes parameter and result types without names.
func writeSignature(b *strings.Builder, s *types.Signature, name typeNamer) {
	b.WriteString("(")
	for i := 0; i < s.Params().Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		writeParam(b, s, i, name)
	}
	b.WriteString(")")
	writeResults(b, s, name)
}

func writeParam(b *strings.Builder, s *types.Signature, i int, name typeNamer) {
	t := s.Params().At(i).Type()
	if s.Variadic() && i == s.Params().Len()-1 {
		b.WriteString("...")
		if sl, ok := t.(*types.Slice); ok {
			t = sl.Elem()
		}
	}
	writeType(b, t, name)
}

func writeResults(b *strings.Builder, s *types.Signature, name typeNamer) {
	n := s.Results().Len()
	if n == 0 {
		return
	}
	b.WriteString(" (")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		writeType(b, s.Results().At(i).Type(), name)
	}
	b.WriteString(")")
}

// mainTypes adds to out the names of all notebook-declared types t mentions
// directly (it does not look inside them: a type that depends on another one
// gets a new version together with it).
func mainTypes(t types.Type, out map[string]bool) {
	seen := map[types.Type]bool{}
	var walk func(types.Type)
	walk = func(t types.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		switch t := t.(type) {
		case *types.Named:
			if p := t.Obj().Pkg(); p != nil && p.Path() == mainPath {
				out[t.Obj().Name()] = true
			}
			for i := 0; i < t.TypeArgs().Len(); i++ {
				walk(t.TypeArgs().At(i))
			}
		case *types.Alias:
			if p := t.Obj().Pkg(); p != nil && p.Path() == mainPath {
				out[t.Obj().Name()] = true
			}
			walk(types.Unalias(t))
		case *types.Pointer:
			walk(t.Elem())
		case *types.Slice:
			walk(t.Elem())
		case *types.Array:
			walk(t.Elem())
		case *types.Chan:
			walk(t.Elem())
		case *types.Map:
			walk(t.Key())
			walk(t.Elem())
		case *types.Struct:
			for i := 0; i < t.NumFields(); i++ {
				walk(t.Field(i).Type())
			}
		case *types.Signature:
			for _, tup := range []*types.Tuple{t.Params(), t.Results()} {
				for i := 0; i < tup.Len(); i++ {
					walk(tup.At(i).Type())
				}
			}
		case *types.Interface:
			for i := 0; i < t.NumMethods(); i++ {
				walk(t.Method(i).Type())
			}
		}
	}
	walk(t)
}
