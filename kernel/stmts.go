package kernel

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strings"

	"github.com/traefik/yaegi/interp"

	"go-notebooks/kernel/cellsrc"
	"go-notebooks/kernel/proto"
)

// execStmts runs consecutive statements of a cell. Variables they define at
// the top level become notebook variables.
func (s *Session) execStmts(cell string, stmts []cellsrc.Chunk) *proto.CellError {
	live := map[string]string{}
	for _, v := range s.vars {
		if !v.stale {
			live[v.name] = v.typeStr
		}
	}
	revived := s.reassigned(stmts, live)

	in := source{vars: live, stmts: stmts, cell: cell}
	for _, u := range sortedUnits(s.units) {
		if u.broken == nil && !strings.HasPrefix(u.key, "init"+mark) {
			in.units = append(in.units, u)
		}
	}
	p, e := s.check(in)
	if e != nil {
		return e
	}
	last := len(stmts) - 1
	for _, err := range p.errs {
		// A trailing expression is shown instead of being an error.
		if pc := p.pieceAt(p.offset(err.Pos)); pc != nil && pc.stmt == last && strings.Contains(err.Msg, "is not used") && !in.display {
			if _, ok := p.stmt(last).(*ast.ExprStmt); ok {
				in.display = true
				if p, e = s.check(in); e != nil {
					return e
				}
				break
			}
		}
	}

	defs := s.definitions(p, revived)
	if e, at := s.errorIn(p, nil); e != nil {
		// The cell may itself recreate what a stale function depends on.
		if e.Kind == proto.ErrStale && at != nil && at.stmt > 0 {
			for _, d := range defs {
				if d.stmt < at.stmt && d.changed {
					if e := s.execStmts(cell, stmts[:at.stmt]); e != nil {
						return e
					}
					return s.execStmts(cell, stmts[at.stmt:])
				}
			}
		}
		return e
	}
	// If a definition changes the meaning of existing functions, they must be
	// updated before the statements after it run.
	for _, d := range defs {
		if d.changed && d.stmt < last && s.usedByUnits(d.v.name) {
			if e := s.execStmts(cell, stmts[:d.stmt+1]); e != nil {
				return e
			}
			return s.execStmts(cell, stmts[d.stmt+1:])
		}
	}

	lo := &lowerer{s: s, p: p, local: map[types.Object]string{}, names: map[string]string{}}
	var vars, out strings.Builder
	for _, d := range defs {
		if d.err != nil {
			return &proto.CellError{Kind: proto.ErrInterp, Line: stmts[d.stmt].Line, Message: fmt.Sprintf("%s cannot be a notebook variable: %v", d.v.name, d.err)}
		}
		lo.local[d.obj] = d.v.internal
		lo.names[d.v.name] = d.v.internal
	}
	for _, d := range defs {
		if d.changed {
			lo.global(&vars, d.v.internal, lo.typ(d.v.typ))
		}
	}
	// Names of the session keep their meaning; only definitions are renamed.
	lo.names = revivedNames(defs)
	run := s.fresh(cellFunc)
	fmt.Fprintf(&out, "func %s() {\n", run)
	for i := range stmts {
		st := p.stmt(i)
		pc := p.pieceOf(i)
		fmt.Fprintf(&out, "%s.Step(%d); ", rtAlias, i)
		s.lowerStmt(lo, st, i == last && in.display, i == last)
		fmt.Fprintf(&out, "%s%s\n", s.lineMark(cell, stmts[i].Line, stmts[i].Col), lo.text(pc.start, pc.end))
	}
	fmt.Fprintf(&out, "%s.Step(%d)\n}\n", rtAlias, len(stmts))
	if lo.err != nil {
		return &proto.CellError{Kind: proto.ErrInterp, Message: "not supported interactively: " + lo.err.Error()}
	}
	if e := s.importYaegi(); e != nil {
		return e
	}
	for _, src := range []string{lo.aliases, vars.String(), out.String()} {
		if src == "" {
			continue
		}
		if err := s.eval(src); err != nil {
			return s.interpError(err)
		}
	}
	e = s.call(run, cell, stmts)

	// Keep the variables whose definition was reached.
	var touched []string
	for _, d := range defs {
		if d.stmt < s.rt.step {
			s.commitVar(d.v, p, d.captures)
			if d.changed {
				touched = append(touched, d.v.name)
			}
		}
	}
	for _, n := range touched {
		if s.usedByUnits(n) || s.brokenUnits() {
			if e2 := s.declare(cell, nil, nil, touched); e == nil {
				e = e2
			}
			break
		}
	}
	return e
}

func (p *program) pieceOf(stmt int) *piece {
	for i := range p.pieces {
		if p.pieces[i].new && p.pieces[i].stmt == stmt {
			return &p.pieces[i]
		}
	}
	return nil
}

func (p *program) cellBody() *ast.FuncDecl {
	for _, d := range p.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == cellFunc {
			return fd
		}
	}
	return nil
}

func (p *program) stmt(i int) ast.Stmt {
	pc := p.pieceOf(i)
	for _, st := range p.cellBody().Body.List {
		if o := p.offset(st.Pos()); o >= pc.start && o <= pc.end {
			return st
		}
	}
	return nil
}

func (s *Session) lowerStmt(lo *lowerer, st ast.Stmt, display, last bool) {
	switch st := st.(type) {
	case nil:
		return
	case *ast.AssignStmt:
		lo.assign(st)
	case *ast.DeclStmt:
		if d, ok := st.Decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
			lo.varDecl(d)
		}
	case *ast.DeferStmt:
		lo.deferStmt(st)
	case *ast.ExprStmt:
		if display {
			c := st.X.(*ast.CallExpr)
			s.showExpr(lo, c.Args[0], c)
		} else if last {
			// Show the result of a trailing call with one result.
			if t := lo.p.info.TypeOf(st.X); t != nil {
				_, isTuple := t.(*types.Tuple)
				tv := lo.p.info.Types[st.X]
				if !isTuple && tv.IsValue() {
					s.showExpr(lo, st.X, nil)
				}
			}
		}
	}
	lo.walk(st)
}

// showExpr wraps the cell's last expression so that its value is reported.
func (s *Session) showExpr(lo *lowerer, x ast.Expr, outer *ast.CallExpr) {
	t := lo.p.info.TypeOf(x)
	if t == nil {
		return
	}
	id := s.rt.typeID(t)
	open, end := fmt.Sprintf("%s.Display(%d, ", rtAlias, id), ")"
	if m, _, _ := types.LookupFieldOrMethod(t, false, lo.p.pkg, "String"); concreteMain(t) && m != nil && stringMethod(t, m) {
		open, end = fmt.Sprintf("%s.Display(%d, (", rtAlias, id), ").String())"
	} else if concreteMain(t) && implementsError(t) {
		open, end = fmt.Sprintf("%s.Display(%d, (", rtAlias, id), ").Error())"
	}
	if outer != nil {
		lo.replace(lo.p.offset(outer.Fun.Pos()), lo.p.offset(x.Pos()), open)
		lo.replace(lo.p.offset(x.End()), lo.p.offset(outer.End()), end)
		return
	}
	lo.before(x, open)
	lo.after(x, end)
}

func stringMethod(t types.Type, m types.Object) bool {
	sig, ok := m.Type().(*types.Signature)
	if !ok || sig.Params().Len() != 0 || sig.Results().Len() != 1 {
		return false
	}
	b, ok := sig.Results().At(0).Type().(*types.Basic)
	if !ok || b.Kind() != types.String {
		return false
	}
	ms := types.NewMethodSet(t)
	return ms.Lookup(m.Pkg(), "String") != nil
}

type definition struct {
	v        *variable
	obj      types.Object
	stmt     int
	changed  bool // needs new storage: first definition, new type, or was stale
	captures map[string]bool
	err      error
}

// definitions finds the variables defined at the top level of the cell.
func (s *Session) definitions(p *program, revived map[string]int) []definition {
	var out []definition
	add := func(id *ast.Ident, stmt int, rhs ast.Node) {
		obj, ok := p.info.Defs[id].(*types.Var)
		if !ok || id.Name == "_" {
			return
		}
		d := definition{obj: obj, stmt: stmt, captures: map[string]bool{}}
		ts, err := typeString(obj.Type(), s.canonical)
		d.err = err
		d.v = &variable{name: id.Name, typ: obj.Type(), typeStr: ts, cell: s.rt.cell}
		o := s.vars[id.Name]
		if o != nil && !o.stale && o.typeStr == ts {
			d.v.internal = o.internal
		} else {
			d.changed = true
			d.v.internal = s.fresh(id.Name)
		}
		if rhs != nil {
			ast.Inspect(rhs, func(n ast.Node) bool {
				if fl, ok := n.(*ast.FuncLit); ok {
					for r := range s.refsOf(p, fl) {
						if s.types[r] != nil {
							d.captures[r] = true
						}
					}
				}
				return true
			})
		}
		out = append(out, d)
	}
	body := p.cellBody()
	if body == nil {
		return nil
	}
	for _, st := range body.Body.List {
		pc := p.pieceAt(p.offset(st.Pos()))
		if pc == nil {
			continue
		}
		switch st := st.(type) {
		case *ast.AssignStmt:
			if st.Tok == token.DEFINE {
				for _, l := range st.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						add(id, pc.stmt, st)
					}
				}
			}
		case *ast.DeclStmt:
			if g, ok := st.Decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, sp := range g.Specs {
					for _, id := range sp.(*ast.ValueSpec).Names {
						add(id, pc.stmt, sp)
					}
				}
			}
		}
	}
	for n, stmt := range revived {
		obj, ok := p.pkg.Scope().Lookup(n).(*types.Var)
		if !ok {
			continue
		}
		ts, err := typeString(obj.Type(), s.canonical)
		out = append(out, definition{obj: obj, stmt: stmt, changed: true, err: err, captures: map[string]bool{},
			v: &variable{name: n, internal: s.fresh(n), typ: obj.Type(), typeStr: ts, cell: s.rt.cell}})
	}
	return out
}

func revivedNames(defs []definition) map[string]string {
	out := map[string]string{}
	for _, d := range defs {
		if d.obj.Parent() != nil && d.obj.Pkg() != nil && d.obj.Parent() == d.obj.Pkg().Scope() {
			out[d.v.name] = d.v.internal
		}
	}
	return out
}

// reassigned finds stale variables whose first use in the cell is a plain
// assignment ("u = User{...}"): such an assignment brings them up to date. The
// variables are added to live with their type in its current version.
func (s *Session) reassigned(stmts []cellsrc.Chunk, live map[string]string) map[string]int {
	out := map[string]int{}
	var stale []string
	for _, v := range s.vars {
		if v.stale {
			stale = append(stale, v.name)
		}
	}
	for _, n := range stale {
		for i, c := range stmts {
			f := parseStmt(c.Text)
			if f == nil || !mentions(f, n) {
				continue
			}
			a, ok := f.(*ast.AssignStmt)
			if ok && a.Tok == token.ASSIGN {
				lhs := false
				for _, l := range a.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name == n {
						lhs = true
					} else if mentions(l, n) {
						lhs = false
						break
					}
				}
				for _, r := range a.Rhs {
					lhs = lhs && !mentions(r, n)
				}
				if lhs {
					out[n] = i
					live[n] = s.vars[n].typeStr
				}
			}
			break
		}
	}
	return out
}

func mentions(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func (s *Session) usedByUnits(name string) bool {
	for _, u := range s.units {
		if u.refs[name] {
			return true
		}
		if u.broken != nil {
			for _, n := range u.broken.names {
				if n == name {
					return true
				}
			}
		}
	}
	return false
}

func (s *Session) brokenUnits() bool {
	for _, u := range s.units {
		if u.broken != nil {
			return true
		}
	}
	return false
}

// call runs an internal function and converts a panic into a cell error.
func (s *Session) call(fn, cell string, stmts []cellsrc.Chunk) *proto.CellError {
	s.trace.take()
	s.rt.step = 0
	_, err := s.in.Eval(fn + "()")
	trace := s.trace.take()
	if err == nil {
		s.rt.step = len(stmts)
		return nil
	}
	e := &proto.CellError{Kind: proto.ErrPanic}
	pe, ok := err.(interp.Panic)
	if !ok {
		e.Kind, e.Message = proto.ErrInterp, s.demangle(err.Error())
		return e
	}
	e.Message = "panic: " + s.panicText(pe.Value)
	for i, n := range trace {
		t := s.linePos(n)
		if t.cell == cell {
			e.Line = t.line
			break
		}
		if i == 0 && t.cell != "" {
			e.Message += fmt.Sprintf(" (in %s, line %d)", s.cellRef(t.cell), t.line)
		}
	}
	if e.Line == 0 && s.rt.step < len(stmts) {
		e.Line = stmts[s.rt.step].Line
	}
	return e
}

func (s *Session) panicText(v any) string {
	if rv, ok := v.(reflect.Value); ok {
		if !rv.IsValid() {
			return "nil"
		}
		v = rv.Interface()
	}
	v = show(v)
	if err, ok := v.(error); ok {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "on zero Value"), strings.Contains(msg, "nil pointer"):
			return "invalid memory address or nil pointer dereference"
		case strings.HasPrefix(msg, "reflect: "):
			msg = strings.TrimPrefix(msg, "reflect: ")
		}
		return s.demangle(strings.TrimPrefix(msg, "runtime error: "))
	}
	if str, ok := v.(string); ok {
		return s.demangle(strings.TrimPrefix(str, "reflect: "))
	}
	return fmt.Sprintf("%+v", v)
}
