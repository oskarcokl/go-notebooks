package kernel

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// edit replaces src[start:end].
type edit struct {
	start, end int
	text       string
	order      int
}

// lowerer turns checked source into the text given to Yaegi by a set of small
// edits, keeping line positions:
//
//   - package-level names become their current internal names and imports
//     their canonical alias;
//   - generic calls are instantiated explicitly (Yaegi's inference is weak);
//   - min, max and clear, which Yaegi lacks, become function literals;
//   - arguments of interface type passed to compiled variadic-any functions
//     such as fmt.Println are unwrapped from Yaegi's representation;
//   - a panic in a goroutine is reported instead of killing the kernel.
type lowerer struct {
	s     *Session
	p     *program
	local map[types.Object]string // cell-level variables -> internal names
	names map[string]string       // overrides of s.names
	edits []edit
	err   error

	aliases string // type declarations needed by global
}

func (l *lowerer) name(n string) string {
	if v, ok := l.names[n]; ok {
		return v
	}
	if v, ok := l.s.names[n]; ok {
		return v
	}
	return n
}

// typeName spells named types for Yaegi using the overrides of l.
func (l *lowerer) typeName(o *types.TypeName) string {
	if o.Pkg() != nil && o.Pkg().Path() == mainPath && o.Parent() == o.Pkg().Scope() {
		return l.name(o.Name())
	}
	return l.s.canonical(o)
}

func (l *lowerer) typ(t types.Type) string {
	str, err := typeString(types.Default(t), l.typeName)
	if err != nil && l.err == nil {
		l.err = err
	}
	return str
}

func (l *lowerer) replace(start, end int, text string) {
	l.edits = append(l.edits, edit{start, end, text, len(l.edits)})
}
func (l *lowerer) before(n ast.Node, text string) {
	o := l.p.offset(n.Pos())
	l.replace(o, o, text)
}

// after inserts behind n; insertions of inner nodes come first.
func (l *lowerer) after(n ast.Node, text string) {
	o := l.p.offset(n.End())
	l.edits = append(l.edits, edit{o, o, text, -len(l.edits) - 1})
}

// text returns src[start:end] with all edits inside it applied.
func (l *lowerer) text(start, end int) string {
	var in []edit
	for _, e := range l.edits {
		if e.start >= start && e.end <= end {
			in = append(in, e)
		}
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.start != b.start {
			return a.start < b.start
		}
		// Insertions come before a replacement at the same place.
		if (a.end == a.start) != (b.end == b.start) {
			return a.end == a.start
		}
		return a.order < b.order
	})
	var b strings.Builder
	pos := start
	for _, e := range in {
		if e.start < pos {
			continue // inside a replaced region
		}
		b.WriteString(l.p.src[pos:e.start])
		b.WriteString(e.text)
		pos = e.end
	}
	b.WriteString(l.p.src[pos:end])
	return b.String()
}

// rename returns the internal spelling of an identifier, or "".
func (l *lowerer) rename(id *ast.Ident) string {
	obj := l.p.info.Uses[id]
	if obj == nil {
		obj = l.p.info.Defs[id]
	}
	if obj == nil {
		return ""
	}
	if n, ok := l.local[obj]; ok {
		return n
	}
	switch o := obj.(type) {
	case *types.PkgName:
		return l.s.alias(o.Imported().Path())
	case *types.Var:
		if o.IsField() {
			// An embedded field is called after its type, which may have an
			// internal name by now.
			if o.Embedded() {
				t := o.Type()
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				if tn := mainTypeName(t); tn != nil {
					return l.name(tn.Name())
				}
			}
			return ""
		}
	}
	if obj.Pkg() != nil && obj.Pkg() == l.p.pkg && obj.Parent() == l.p.pkg.Scope() {
		return l.name(obj.Name())
	}
	return ""
}

func mainTypeName(t types.Type) *types.TypeName {
	switch t := t.(type) {
	case *types.Named:
		if p := t.Obj().Pkg(); p != nil && p.Path() == mainPath {
			return t.Obj()
		}
	case *types.Alias:
		if p := t.Obj().Pkg(); p != nil && p.Path() == mainPath {
			return t.Obj()
		}
	}
	return nil
}

// walk records the edits common to all code in n.
func (l *lowerer) walk(n ast.Node) {
	var stack []ast.Node
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == displayFunc {
				break
			}
			if to := l.rename(n); to != "" && to != n.Name {
				l.replace(l.p.offset(n.Pos()), l.p.offset(n.End()), to)
			}
			l.instantiate(n, stack)
		case *ast.CallExpr:
			l.call(n)
		case *ast.GoStmt:
			l.goStmt(n)
		case *ast.RangeStmt:
			if t := l.p.info.TypeOf(n.X); t != nil {
				if _, ok := t.Underlying().(*types.Signature); ok && l.err == nil {
					l.err = fmt.Errorf("ranging over functions is only available in go build")
				}
			}
		}
		return true
	})
}

// instantiate makes inferred type arguments of notebook generics explicit.
func (l *lowerer) instantiate(id *ast.Ident, stack []ast.Node) {
	inst, ok := l.p.info.Instances[id]
	if !ok || inst.TypeArgs.Len() == 0 {
		return
	}
	if _, isFunc := l.p.info.Uses[id].(*types.Func); !isFunc {
		return
	}
	if len(stack) >= 2 {
		switch x := stack[len(stack)-2].(type) {
		case *ast.IndexExpr:
			if x.X == id {
				return
			}
		case *ast.IndexListExpr:
			return
		case *ast.SelectorExpr:
			return
		}
	}
	args := make([]string, inst.TypeArgs.Len())
	for i := range args {
		args[i] = l.typ(inst.TypeArgs.At(i))
	}
	l.after(id, "["+strings.Join(args, ", ")+"]")
}

func (l *lowerer) call(c *ast.CallExpr) {
	if id, ok := ast.Unparen(c.Fun).(*ast.Ident); ok {
		if b, ok := l.p.info.Uses[id].(*types.Builtin); ok {
			l.builtin(b.Name(), c)
			return
		}
	}
	sig, ok := typeUnder(l.p.info.TypeOf(c.Fun)).(*types.Signature)
	if !ok {
		return
	}
	fn := calleeObj(l.p.info, c.Fun)
	if fn == nil || fn.Pkg() == nil || fn.Pkg() == l.p.pkg {
		return
	}
	for i, arg := range c.Args {
		at := l.p.info.TypeOf(arg)
		if at == nil || l.p.info.Types[arg].IsNil() {
			continue
		}
		spread := c.Ellipsis.IsValid() && i == len(c.Args)-1
		var pt types.Type
		if n := sig.Params().Len(); sig.Variadic() && i >= n-1 {
			pt = sig.Params().At(n - 1).Type()
			if sl, ok := pt.(*types.Slice); ok && !spread {
				pt = sl.Elem()
			}
		} else if i < n {
			pt = sig.Params().At(i).Type()
		}
		if pt == nil {
			continue
		}
		if spread {
			if sl, ok := typeUnder(at).(*types.Slice); ok && isEmptyInterface(sl.Elem()) {
				l.wrap(arg, "ShowAll")
			}
			continue
		}
		if !isEmptyInterface(pt) {
			continue
		}
		switch {
		case holdsInterface(at):
			l.wrap(arg, "Show")
		case fn.Pkg().Path() == "fmt" && fn.Name() == "Errorf" && implementsError(at) && concreteMain(at):
			l.wrap(arg, "Err")
		}
	}
}

func (l *lowerer) wrap(arg ast.Expr, fn string) {
	l.before(arg, rtAlias+"."+fn+"(")
	l.after(arg, ")")
}

func typeUnder(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	return t.Underlying()
}

func calleeObj(info *types.Info, fun ast.Expr) types.Object {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return info.Uses[f]
	case *ast.SelectorExpr:
		return info.Uses[f.Sel]
	}
	return nil
}

func isEmptyInterface(t types.Type) bool {
	i, ok := typeUnder(t).(*types.Interface)
	return ok && i.Empty()
}

// holdsInterface reports whether values of type t may contain Yaegi's internal
// wrapper for dynamic values: any and notebook-defined interfaces, also inside
// slices and maps.
func holdsInterface(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Interface:
		if u.Empty() {
			return true
		}
		return mainTypeName(t) != nil
	case *types.Slice:
		return holdsInterface(u.Elem())
	case *types.Map:
		return holdsInterface(u.Elem())
	}
	return false
}

func concreteMain(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	_, isIface := t.Underlying().(*types.Interface)
	return !isIface && mainTypeName(t) != nil
}

var errorIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

func implementsError(t types.Type) bool { return types.Implements(t, errorIface) }

func (l *lowerer) builtin(name string, c *ast.CallExpr) {
	start, end := l.p.offset(c.Fun.Pos()), l.p.offset(c.Fun.End())
	switch name {
	case "min", "max":
		t := l.p.info.TypeOf(c)
		if b, ok := t.(*types.Basic); ok && b.Info()&types.IsUntyped != 0 {
			t = types.Default(t)
		}
		ts := l.typ(t)
		op := "<"
		if name == "max" {
			op = ">"
		}
		var ps, body string
		for i := range c.Args {
			if i > 0 {
				ps += ", "
				body += fmt.Sprintf("if p%d %s p0 { p0 = p%d }; ", i, op, i)
			}
			ps += fmt.Sprintf("p%d", i)
		}
		l.replace(start, end, fmt.Sprintf("func(%s %s) %s { %sreturn p0 }", ps, ts, ts, body))
	case "clear":
		t := l.p.info.TypeOf(c.Args[0])
		ts := l.typ(t)
		if sl, ok := t.Underlying().(*types.Slice); ok {
			l.replace(start, end, fmt.Sprintf("func(m %s) { var z %s; for i := range m { m[i] = z } }", ts, l.typ(sl.Elem())))
		} else {
			l.replace(start, end, fmt.Sprintf("func(m %s) { for k := range m { delete(m, k) } }", ts))
		}
	}
}

const goGuard = "defer func() { " + rtAlias + ".GoDone(); if r := recover(); r != nil { " + rtAlias + ".GoPanic(r) } }(); "

func (l *lowerer) goStmt(g *ast.GoStmt) {
	start, call := l.p.offset(g.Pos()), l.p.offset(g.Call.Pos())
	if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
		l.replace(start, call, "{ "+rtAlias+".GoStart(); go ")
		o := l.p.offset(lit.Body.Lbrace) + 1
		l.replace(o, o, " "+goGuard)
		l.after(g, " }")
		return
	}
	temps := l.captureArgs(g.Call, false)
	l.replace(start, call, "{ "+temps+rtAlias+".GoStart(); go func() { "+goGuard)
	l.after(g, " }() }")
}

// captureArgs evaluates the arguments of a call into temporaries, as go and
// defer do, and returns the declarations.
func (l *lowerer) captureArgs(c *ast.CallExpr, recv bool) string {
	var out string
	if sel, ok := c.Fun.(*ast.SelectorExpr); ok && recv {
		if s := l.p.info.Selections[sel]; s != nil {
			switch typeUnder(l.p.info.TypeOf(sel.X)).(type) {
			case *types.Pointer, *types.Interface:
				out += l.capture(sel.X, "r"+mark)
			}
		}
	}
	for i, a := range c.Args {
		tv := l.p.info.Types[a]
		if tv.Value != nil || tv.IsNil() || tv.IsType() {
			continue
		}
		if _, isLit := a.(*ast.FuncLit); isLit {
			continue
		}
		if _, isTuple := tv.Type.(*types.Tuple); isTuple {
			continue
		}
		out += l.capture(a, fmt.Sprintf("a%s%d", mark, i))
	}
	return out
}

func (l *lowerer) capture(e ast.Expr, tmp string) string {
	sub := &lowerer{s: l.s, p: l.p, local: l.local, names: l.names}
	sub.walk(e)
	start, end := l.p.offset(e.Pos()), l.p.offset(e.End())
	text := sub.text(start, end)
	if sub.err != nil && l.err == nil {
		l.err = sub.err
	}
	l.replace(start, end, tmp)
	return tmp + " := " + text + "; "
}

// deferStmt keeps a cell-level defer pending until the kernel stops or the
// cell is run again, like a defer in main would.
func (l *lowerer) deferStmt(d *ast.DeferStmt) {
	start, call := l.p.offset(d.Pos()), l.p.offset(d.Call.Pos())
	temps := l.captureArgs(d.Call, true)
	l.replace(start, call, "{ "+temps+rtAlias+".Defer(func() { ")
	l.after(d, " }) }")
}

// assign turns a cell-level definition into an assignment.
func (l *lowerer) assign(a *ast.AssignStmt) {
	if a.Tok == token.DEFINE {
		o := l.p.offset(a.TokPos)
		l.replace(o, o+2, "=")
	}
}

// varDecl turns a cell-level var declaration into assignments.
func (l *lowerer) varDecl(d *ast.GenDecl) {
	if d.Lparen.IsValid() {
		l.replace(l.p.offset(d.Pos()), l.p.offset(d.Lparen)+1, "{")
		o := l.p.offset(d.Rparen)
		l.replace(o, o+1, "}")
	} else {
		l.replace(l.p.offset(d.Pos()), l.p.offset(d.Specs[0].Pos()), "")
	}
	for _, sp := range d.Specs {
		v := sp.(*ast.ValueSpec)
		var names []string
		for _, id := range v.Names {
			n := id.Name
			if n != "_" {
				n = l.rename(id)
			}
			names = append(names, n)
		}
		lhs := strings.Join(names, ", ")
		start := l.p.offset(v.Pos())
		if len(v.Values) > 0 {
			l.replace(start, l.p.offset(v.Values[0].Pos()), lhs+" = ")
			continue
		}
		var text []string
		for i, id := range v.Names {
			if id.Name != "_" {
				text = append(text, fmt.Sprintf("{ var z%s %s; %s = z%s }", mark, l.typ(l.p.info.TypeOf(v.Type)), names[i], mark))
			}
		}
		l.replace(start, l.p.offset(v.End()), strings.Join(text, "; "))
	}
}
