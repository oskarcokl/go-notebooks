package kernel

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"go-notebooks/kernel/cellsrc"
	"go-notebooks/kernel/proto"
)

const (
	cellFunc    = "cell" + mark
	displayFunc = "display" + mark
)

// piece is a stretch of a check program.
type piece struct {
	unit       *unit // nil for variables and statements
	new        bool  // part of the cell being run
	stmt       int   // statement index, or -1
	start, end int   // byte offsets
}

// program is one type-checked snapshot: the session plus the new code.
type program struct {
	fset   *token.FileSet
	src    string
	file   *ast.File
	info   *types.Info
	pkg    *types.Package
	pieces []piece
	errs   []types.Error
}

func (p *program) offset(pos token.Pos) int { return p.fset.PositionFor(pos, false).Offset }

func (p *program) pieceAt(off int) *piece {
	for i := range p.pieces {
		if off >= p.pieces[i].start && off <= p.pieces[i].end {
			return &p.pieces[i]
		}
	}
	return nil
}

func (p *program) within(n ast.Node, pc *piece) bool {
	return p.offset(n.Pos()) >= pc.start && p.offset(n.End()) <= pc.end
}

// source is what goes into a program.
type source struct {
	units   []*unit // session declarations
	fresh   map[*unit]bool
	vars    map[string]string // name -> type
	pkgVars []cellsrc.Chunk   // package-level var declarations of the cell
	stmts   []cellsrc.Chunk
	display bool // the last statement is an expression to show
	cell    string
}

func (s *Session) check(in source) (*program, *proto.CellError) {
	p := &program{fset: token.NewFileSet()}
	var b strings.Builder
	b.WriteString("package main\n")
	var names []string
	for n := range s.imports {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		// A variable declared after the import shadows it, as it would in main.
		if _, isVar := s.vars[n]; isVar {
			continue
		}
		fmt.Fprintf(&b, "import %s %q\n", n, s.imports[n])
	}
	var paths []string
	for path := range s.aliases {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		fmt.Fprintf(&b, "import %s %q\n", s.aliases[path], path)
	}
	fmt.Fprintf(&b, "func %s(any) {}\n", displayFunc)
	var vnames []string
	for n := range in.vars {
		vnames = append(vnames, n)
	}
	sort.Strings(vnames)
	for _, n := range vnames {
		fmt.Fprintf(&b, "var %s %s\n", n, in.vars[n])
	}
	add := func(pc piece, text, cell string, line, col int) {
		fmt.Fprintf(&b, "/*line %s:%d:%d*/", cell, line, col)
		pc.start = b.Len()
		b.WriteString(text)
		pc.end = b.Len()
		p.pieces = append(p.pieces, pc)
		b.WriteString("\n")
	}
	for _, u := range in.units {
		text := u.text
		if in.fresh[u] {
			text = u.raw
		}
		add(piece{unit: u, new: in.fresh[u], stmt: -1}, text, u.cell, u.line, u.col)
	}
	for _, c := range in.pkgVars {
		add(piece{new: true, stmt: -1}, c.Text, in.cell, c.Line, c.Col)
	}
	if in.stmts != nil {
		fmt.Fprintf(&b, "func %s() {\n", cellFunc)
		for i, c := range in.stmts {
			text := c.Text
			if in.display && i == len(in.stmts)-1 {
				text = displayFunc + "(" + text + ")"
			}
			add(piece{new: true, stmt: i}, text, in.cell, c.Line, c.Col)
		}
		b.WriteString("}\n")
	}
	p.src = b.String()

	f, err := parser.ParseFile(p.fset, "session", p.src, parser.SkipObjectResolution)
	if err != nil {
		var list scanner.ErrorList
		if errors.As(err, &list) && len(list) > 0 {
			return nil, &proto.CellError{Kind: proto.ErrSyntax, Message: "syntax error: " + s.demangle(list[0].Msg), Line: list[0].Pos.Line}
		}
		return nil, syntaxError(err)
	}
	p.file = f
	p.info = &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances: map[*ast.Ident]types.Instance{}, Scopes: map[ast.Node]*types.Scope{},
	}
	conf := types.Config{
		Importer: s.imp,
		Error: func(err error) {
			var te types.Error
			// Soft errors are unused variables, imports and labels: allowed here.
			if errors.As(err, &te) && !te.Soft {
				p.errs = append(p.errs, te)
			}
		},
	}
	p.pkg, _ = conf.Check(mainPath, p.fset, []*ast.File{f}, p.info)
	// A declaration that is out of service must not uncover the builtin it
	// shadows, such as a func called print or max.
	var hidden []types.Error
	for id, obj := range p.info.Uses {
		if obj.Parent() == types.Universe && s.names[id.Name] != "" && p.pkg.Scope().Lookup(id.Name) == nil {
			hidden = append(hidden, types.Error{Fset: p.fset, Pos: id.Pos(), Msg: "undefined: " + id.Name})
		}
	}
	sort.Slice(hidden, func(i, j int) bool { return hidden[i].Pos < hidden[j].Pos })
	p.errs = append(p.errs, hidden...)
	return p, nil
}

func undefinedName(msg string) string {
	n, ok := strings.CutPrefix(msg, "undefined: ")
	if !ok {
		return ""
	}
	return n
}

// errorIn converts the errors found in new code to a cell error, nil if none.
// References to stale variables and broken functions get an explanation.
func (s *Session) errorIn(p *program, excluded map[string]bool) (*proto.CellError, *piece) {
	var first *types.Error
	var at *piece
	var stale []proto.Stale
	seen := map[string]bool{}
	line := 0
	for i := range p.errs {
		e := &p.errs[i]
		pc := p.pieceAt(p.offset(e.Pos))
		if pc == nil || !pc.new {
			continue
		}
		if n := undefinedName(e.Msg); n != "" {
			if seen[n] {
				continue
			}
			seen[n] = true
			if st, ok := s.staleReason(n, excluded, map[string]bool{}); ok {
				stale = append(stale, st...)
				if line == 0 {
					line, at = p.fset.Position(e.Pos).Line, pc
				}
				continue
			}
		}
		// A method that could not follow a type change is gone from the type.
		if u := s.brokenMethod(e.Msg); u != nil {
			if st, ok := s.staleReason(u.key, excluded, map[string]bool{}); ok {
				stale = append(stale, st...)
				if line == 0 {
					line, at = p.fset.Position(e.Pos).Line, pc
				}
				continue
			}
		}
		if first == nil {
			first = e
			if len(stale) == 0 {
				at = pc
			}
		}
	}
	if len(stale) > 0 {
		return staleError(stale, line), at
	}
	if first == nil {
		// Errors outside any piece come from the session itself.
		for i := range p.errs {
			if p.pieceAt(p.offset(p.errs[i].Pos)) == nil {
				return &proto.CellError{Kind: proto.ErrCompile, Message: s.demangle(p.errs[i].Msg)}, nil
			}
		}
		return nil, nil
	}
	msg := first.Msg
	if n := undefinedName(msg); n != "" {
		if path := s.guessImport(n); path != "" {
			msg += fmt.Sprintf(" (missing import %q?)", path)
		}
	}
	return &proto.CellError{Kind: proto.ErrCompile, Message: s.demangle(msg), Line: p.fset.Position(first.Pos).Line}, at
}

func (s *Session) brokenMethod(msg string) *unit {
	for _, u := range sortedUnits(s.units) {
		if u.method != "" && u.broken != nil && missingMethod(msg, u) {
			return u
		}
	}
	return nil
}

func (s *Session) guessImport(name string) string {
	best := ""
	for path, n := range s.manifest.Names {
		if n != name || strings.Contains(path, "internal/") || strings.HasPrefix(path, "vendor/") {
			continue
		}
		if best == "" || len(path) < len(best) || len(path) == len(best) && path < best {
			best = path
		}
	}
	return best
}

func staleError(list []proto.Stale, line int) *proto.CellError {
	var msgs []string
	seen := map[string]bool{}
	var uniq []proto.Stale
	for _, st := range list {
		if !seen[st.Name] {
			seen[st.Name] = true
			uniq = append(uniq, st)
			msgs = append(msgs, st.Reason)
		}
	}
	return &proto.CellError{Kind: proto.ErrStale, Message: strings.Join(msgs, "\n"), Line: line, Stale: uniq}
}

// staleReason explains why name cannot be used at the moment.
func (s *Session) staleReason(name string, excluded, seen map[string]bool) ([]proto.Stale, bool) {
	if seen[name] {
		return nil, false
	}
	seen[name] = true
	if v := s.vars[name]; v != nil && (v.stale || excluded[name]) {
		return []proto.Stale{s.staleVar(v)}, true
	}
	u := s.units[name]
	if u == nil {
		for _, x := range s.units { // const groups
			for _, n := range x.names {
				if n == name {
					u = x
				}
			}
		}
	}
	if u == nil || u.broken == nil {
		return nil, false
	}
	var out []proto.Stale
	for _, n := range u.broken.names {
		if st, ok := s.staleReason(n, excluded, seen); ok {
			for _, x := range st {
				x.Reason = fmt.Sprintf("`%s` uses `%s`: %s", name, n, x.Reason)
				out = append(out, x)
			}
		}
	}
	if len(out) > 0 {
		return out, true
	}
	st := proto.Stale{Name: name, CellID: u.cell, Cell: s.cells[u.cell]}
	st.Reason = fmt.Sprintf("`%s` no longer compiles after other cells changed (%s) — edit and re-run %s", name, u.broken.msg, s.cellRef(u.cell))
	return []proto.Stale{st}, true
}

func (s *Session) cellRef(id string) string {
	if n := s.cells[id]; n > 0 {
		return fmt.Sprintf("cell #%d", n)
	}
	return "the cell that defines it"
}

// staleVar describes a stale variable.
func (s *Session) staleVar(v *variable) proto.Stale {
	st := proto.Stale{Name: v.name, CellID: v.cell, Cell: s.cells[v.cell], Changes: s.changesFor(v)}
	var what []string
	for _, c := range st.Changes {
		noun := "type"
		if ti := s.types[c.Type]; ti != nil && ti.versions[len(ti.versions)-1].isStruct {
			noun = "struct"
		}
		what = append(what, fmt.Sprintf("underlying %s `%s` changed (v%d -> v%d: %s)", noun, c.Type, c.From, c.To, strings.Join(c.Details, ", ")))
	}
	if len(what) == 0 {
		what = []string{"a type it depends on changed"}
	}
	do := fmt.Sprintf("re-run cell #%d to update `%s`", st.Cell, v.name)
	if st.Cell == 0 {
		do = fmt.Sprintf("assign `%s` again to update it", v.name)
	}
	st.Reason = fmt.Sprintf("%s; variable `%s` is stale — %s", strings.Join(what, "; "), v.name, do)
	return st
}

// changesFor lists the differences between the type versions v was built from
// and the current ones.
func (s *Session) changesFor(v *variable) []proto.Change {
	var out []proto.Change
	for name, ver := range v.versions {
		ti := s.types[name]
		if ti == nil || ti.version() <= ver {
			continue
		}
		out = append(out, proto.Change{Type: name, From: ver, To: ti.version(),
			Details: diffSchema(ti.versions[ver-1], ti.versions[ti.version()-1])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	// Prefer the types that were edited over those that merely contain them.
	var direct []proto.Change
	for _, c := range out {
		if len(c.Details) != 1 || !strings.HasPrefix(c.Details[0], dependsOn) {
			direct = append(direct, c)
		}
	}
	if len(direct) > 0 {
		return direct
	}
	return out
}

const dependsOn = "a type it contains changed"

func diffSchema(a, b schema) []string {
	var out []string
	if !a.isStruct || !b.isStruct {
		if a.text != b.text {
			return []string{fmt.Sprintf("was %s, now %s", a.text, b.text)}
		}
		return []string{dependsOn}
	}
	old := map[string]proto.Field{}
	for _, f := range a.fields {
		old[f.Name] = f
	}
	now := map[string]bool{}
	for _, f := range b.fields {
		now[f.Name] = true
		o, ok := old[f.Name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("added %s %s", f.Name, f.Type))
		case o.Type != f.Type:
			out = append(out, fmt.Sprintf("changed %s: %s -> %s", f.Name, o.Type, f.Type))
		case o.Tag != f.Tag:
			out = append(out, fmt.Sprintf("changed tag of %s", f.Name))
		}
	}
	for _, f := range a.fields {
		if !now[f.Name] {
			out = append(out, fmt.Sprintf("removed %s %s", f.Name, f.Type))
		}
	}
	if len(out) == 0 {
		switch {
		case a.text != b.text:
			out = []string{"reordered fields"}
		default:
			out = []string{dependsOn}
		}
	}
	return out
}

func (s *Session) schemaOf(t types.Type) schema {
	u := t.Underlying()
	st, ok := u.(*types.Struct)
	sc := schema{text: s.displayType(u)}
	if !ok {
		return sc
	}
	sc.isStruct = true
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		sc.fields = append(sc.fields, proto.Field{Name: f.Name(), Type: s.displayType(f.Type()), Tag: st.Tag(i)})
	}
	return sc
}
