package kernel

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go-notebooks/kernel/cellsrc"
	"go-notebooks/kernel/proto"
)

// parseUnits turns declaration chunks into units and package variables, and
// records imports.
func (s *Session) parseUnits(cell string, chunks []cellsrc.Chunk) (units []*unit, vars []cellsrc.Chunk, e *proto.CellError) {
	for _, c := range chunks {
		src := "package p;" + fmt.Sprintf("/*line %s:%d:%d*/", cell, c.Line, c.Col) + c.Text
		base := len(src) - len(c.Text)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
		if err != nil {
			if list, ok := err.(scanner.ErrorList); ok && len(list) > 0 {
				return nil, nil, &proto.CellError{Kind: proto.ErrSyntax, Message: "syntax error: " + list[0].Msg, Line: list[0].Pos.Line}
			}
			return nil, nil, syntaxError(err)
		}
		if c.Kind == cellsrc.Var {
			vars = append(vars, c)
			continue
		}
		slice := func(n ast.Node) string {
			off := func(p token.Pos) int { return fset.PositionFor(p, false).Offset }
			return src[off(n.Pos()):off(n.End())]
		}
		newUnit := func(key, text string, n ast.Node, names ...string) *unit {
			p := fset.Position(n.Pos())
			u := &unit{key: key, kind: c.Kind, names: names, raw: text, cell: cell, line: p.Line, col: p.Column}
			units = append(units, u)
			return u
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				u := newUnit(d.Name.Name, src[base:], d, d.Name.Name)
				u.generic = d.Type.TypeParams != nil
				if d.Recv != nil && len(d.Recv.List) == 1 {
					u.names = nil
					u.method = d.Name.Name
					t := d.Recv.List[0].Type
					if st, ok := t.(*ast.StarExpr); ok {
						t = st.X
					}
					switch x := t.(type) {
					case *ast.IndexExpr:
						t, u.generic = x.X, true
					case *ast.IndexListExpr:
						t, u.generic = x.X, true
					}
					if id, ok := t.(*ast.Ident); ok {
						u.recv = id.Name
					}
					u.key = u.recv + "." + u.method
				} else if d.Name.Name == "init" {
					u.key = "init" + mark + cell + ":" + strconv.Itoa(u.line)
					u.names = nil
				} else if d.Name.Name == "_" {
					units = units[:len(units)-1]
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.IMPORT:
					for _, sp := range d.Specs {
						if e := s.addImport(sp.(*ast.ImportSpec), fset); e != nil {
							return nil, nil, e
						}
					}
				case token.TYPE:
					for _, sp := range d.Specs {
						ts := sp.(*ast.TypeSpec)
						u := newUnit(ts.Name.Name, "type "+slice(ts), ts, ts.Name.Name)
						u.generic = ts.TypeParams != nil
					}
				case token.CONST:
					var names []string
					for _, sp := range d.Specs {
						for _, id := range sp.(*ast.ValueSpec).Names {
							if id.Name != "_" {
								names = append(names, id.Name)
							}
						}
					}
					if len(names) > 0 {
						newUnit("const:"+names[0], src[base:], d, names...)
					}
				}
			}
		}
	}
	return units, vars, nil
}

func (s *Session) addImport(sp *ast.ImportSpec, fset *token.FileSet) *proto.CellError {
	path, _ := strconv.Unquote(sp.Path.Value)
	line := fset.Position(sp.Pos()).Line
	if path == "C" {
		return &proto.CellError{Kind: proto.ErrCompile, Line: line, Message: `import "C" cannot be used in a cell; put the cgo code in a package and import that`}
	}
	if s.manifest.Exports[path] == "" && path != "unsafe" {
		return &proto.CellError{Kind: proto.ErrNeedSync, Line: line, Imports: []string{path},
			Message: fmt.Sprintf("package %q is not part of the running kernel — sync dependencies to add it", path)}
	}
	name := ""
	if sp.Name != nil {
		name = sp.Name.Name
	}
	switch name {
	case "_": // linked into the kernel: its init functions have run
		return s.needLinked(path, line)
	case ".":
		return &proto.CellError{Kind: proto.ErrCompile, Line: line, Message: "dot imports are not supported in notebooks"}
	case "":
		name = s.pkgName(path)
	}
	if path != "unsafe" {
		if e := s.needLinked(path, line); e != nil {
			return e
		}
		if !s.symbols[path] {
			return &proto.CellError{Kind: proto.ErrInterp, Line: line, Message: fmt.Sprintf("package %q has no symbols usable interactively", path)}
		}
	}
	s.imports[name] = path
	s.alias(path)
	return s.importYaegi()
}

// needLinked asks for a sync if a package is known to the type checker (as a
// dependency of another one) but was not extracted into this kernel.
func (s *Session) needLinked(path string, line int) *proto.CellError {
	if s.symbols[path] || !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") && s.isStd(path) {
		return nil
	}
	for _, p := range s.manifest.Packages {
		if p == path {
			return nil
		}
	}
	return &proto.CellError{Kind: proto.ErrNeedSync, Line: line, Imports: []string{path},
		Message: fmt.Sprintf("package %q is not part of the running kernel — sync dependencies to add it", path)}
}

func (s *Session) isStd(path string) bool {
	for _, p := range s.manifest.Std {
		if p == path {
			return true
		}
	}
	return false
}

// tokens returns src without comments and layout, to see whether code changed.
func tokens(src string) string {
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var sc scanner.Scanner
	sc.Init(file, []byte(src), func(token.Position, string) {}, 0)
	var b strings.Builder
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			return strings.TrimRight(b.String(), ";\n ")
		}
		if tok == token.SEMICOLON {
			lit = ";"
		}
		if lit == "" {
			lit = tok.String()
		}
		b.WriteString(lit + " ")
	}
}

// execDecls runs a run of declarations.
func (s *Session) execDecls(cell string, chunks []cellsrc.Chunk) *proto.CellError {
	units, vars, e := s.parseUnits(cell, chunks)
	if e != nil {
		return e
	}
	seen := map[string]*unit{}
	for _, u := range units {
		if o := seen[u.key]; o != nil {
			return &proto.CellError{Kind: proto.ErrCompile, Line: u.line, Message: fmt.Sprintf("%s redeclared in this cell (other declaration on line %d)", strings.SplitN(u.key, mark, 2)[0], o.line)}
		}
		seen[u.key] = u
	}
	return s.declare(cell, units, vars, nil)
}

// declare installs new or changed declarations, and brings everything that
// depends on them up to date. touched lists names that already changed.
func (s *Session) declare(cell string, units []*unit, pkgVars []cellsrc.Chunk, touched []string) *proto.CellError {
	// Declarations that did not change are left alone.
	changed := units[:0:0]
	for _, u := range units {
		if o := s.units[u.key]; o != nil && o.broken == nil && o.method == u.method && tokens(o.raw) == tokens(u.raw) && u.key[:min(4, len(u.key))] != "init" {
			o.cell, o.line, o.col, o.raw = u.cell, u.line, u.col, u.raw
			for _, n := range u.names {
				if ti := s.types[n]; ti != nil {
					ti.cell = u.cell
				}
			}
			continue
		}
		changed = append(changed, u)
	}
	if len(changed) == 0 && len(pkgVars) == 0 && len(touched) == 0 {
		return nil
	}
	force := map[string]bool{} // types that need a new version for other reasons
	for range len(s.units) + len(changed) + 2 {
		again, e := s.tryDeclare(cell, changed, pkgVars, touched, force)
		if e != nil || !again {
			return e
		}
	}
	return &proto.CellError{Kind: proto.ErrInterp, Message: "internal error: declarations do not settle"}
}

func (s *Session) tryDeclare(cell string, fresh []*unit, pkgVars []cellsrc.Chunk, touched []string, force map[string]bool) (again bool, e *proto.CellError) {
	isFresh := map[*unit]bool{}
	replaced := map[string]bool{} // keys
	freshNames := map[string]bool{}
	for _, u := range fresh {
		isFresh[u] = true
		replaced[u.key] = true
		for _, n := range u.names {
			freshNames[n] = true
		}
	}
	var old []*unit
	for _, u := range sortedUnits(s.units) {
		if replaced[u.key] {
			continue
		}
		clash := false // the name now means something else
		for _, n := range u.names {
			clash = clash || freshNames[n]
		}
		if !clash && !strings.HasPrefix(u.key, "init"+mark) {
			old = append(old, u)
		}
	}

	// Types and constants whose meaning changes, transitively.
	bumped := map[string]bool{} // type and const names
	for n := range force {
		bumped[n] = true
	}
	for _, u := range fresh {
		if u.kind == cellsrc.Type || u.kind == cellsrc.Const {
			for _, n := range u.names {
				bumped[n] = true
			}
		}
	}
	for again := true; again; {
		again = false
		for _, u := range old {
			if u.kind != cellsrc.Type && u.kind != cellsrc.Const || bumped[u.names[0]] {
				continue
			}
			for r := range u.refs {
				if bumped[r] {
					for _, n := range u.names {
						bumped[n] = true
					}
					again = true
					break
				}
			}
		}
	}

	// Variables that hold values of types about to be replaced.
	excluded := map[string]bool{}
	live := map[string]string{}
	for _, v := range s.sortedVars() {
		if v.stale || freshNames[v.name] {
			continue
		}
		refs := map[string]bool{}
		mainTypes(v.typ, refs)
		for c := range v.captures {
			refs[c] = true
		}
		for r := range refs {
			if bumped[r] && s.types[r] != nil {
				excluded[v.name] = true
			}
		}
		if !excluded[v.name] {
			live[v.name] = v.typeStr
		}
	}
	newVars, e := s.pkgVarNames(cell, pkgVars)
	if e != nil {
		return false, e
	}
	for _, n := range newVars {
		delete(live, n)
		kept := old[:0]
		for _, u := range old {
			if len(u.names) != 1 || u.names[0] != n {
				kept = append(kept, u)
			}
		}
		old = kept
	}

	// Check, dropping old declarations that no longer compile.
	broke := map[*unit]*broken{}
	var p *program
	for {
		var in []*unit
		for _, u := range old {
			if broke[u] == nil {
				in = append(in, u)
			}
		}
		in = append(in, fresh...)
		p, e = s.check(source{units: in, fresh: isFresh, vars: live, pkgVars: pkgVars, cell: cell})
		if e != nil {
			return false, e
		}
		dropped := false
		for _, err := range p.errs {
			pc := p.pieceAt(p.offset(err.Pos))
			if pc == nil || pc.unit == nil || pc.new {
				continue
			}
			b := broke[pc.unit]
			if b == nil {
				b = &broken{msg: s.demangle(err.Msg)}
				broke[pc.unit] = b
				dropped = true
			}
			if n := undefinedName(err.Msg); n != "" {
				b.names = append(b.names, n)
			}
		}
		if dropped {
			// Variables of a type that no longer compiles go with it.
			for u := range broke {
				for _, v := range s.vars {
					refs := map[string]bool{}
					mainTypes(v.typ, refs)
					if u.kind == cellsrc.Type && refs[u.names[0]] && live[v.name] != "" {
						excluded[v.name] = true
						delete(live, v.name)
					}
				}
			}
			continue
		}
		if e, _ := s.errorIn(p, excluded); e != nil {
			return false, e
		}
		break
	}

	// Decide what must be given to Yaegi again.
	changedNames := map[string]bool{}
	for n := range bumped {
		changedNames[n] = true
	}
	for _, n := range touched {
		changedNames[n] = true
	}
	for n := range excluded {
		changedNames[n] = true
	}
	names := map[string]string{} // new internal names
	for _, n := range sortedKeys(bumped) {
		names[n] = s.fresh(n)
	}
	type newVar struct {
		v    *variable
		keep bool
	}
	var vars []newVar
	for _, n := range newVars {
		obj := p.pkg.Scope().Lookup(n)
		if obj == nil {
			continue
		}
		ts, err := typeString(obj.Type(), s.canonical)
		if err != nil {
			return false, &proto.CellError{Kind: proto.ErrInterp, Message: fmt.Sprintf("%s cannot be a notebook variable: %v", n, err)}
		}
		v := &variable{name: n, typ: obj.Type(), typeStr: ts, cell: cell}
		o := s.vars[n]
		keep := o != nil && !o.stale && !excluded[n] && o.typeStr == ts && !freshNames[n]
		if keep {
			v.internal = o.internal
		} else {
			v.internal = s.fresh(n)
			changedNames[n] = true
		}
		names[n] = v.internal
		vars = append(vars, newVar{v, keep})
	}
	var all []*unit
	for _, u := range append(append([]*unit{}, old...), fresh...) {
		if broke[u] == nil {
			all = append(all, u)
		}
	}
	lo := &lowerer{s: s, p: p, names: names}
	sigs := map[*unit]string{}
	dirty := map[*unit]bool{}
	refs := map[*unit]map[string]bool{}
	decls := map[*unit]ast.Decl{}
	s.unitDecls(p, decls)
	for _, u := range all {
		if decls[u] == nil {
			return false, &proto.CellError{Kind: proto.ErrInterp, Message: "internal error: lost declaration of " + u.key}
		}
		refs[u] = s.refsOf(p, decls[u])
		if isFresh[u] || u.broken != nil {
			dirty[u] = true
			if u.kind == cellsrc.Func && u.method == "" && s.units[u.key] == nil {
				for _, n := range u.names {
					changedNames[n] = true
				}
			}
		}
		if u.kind == cellsrc.Type && bumped[u.names[0]] || u.kind == cellsrc.Const && bumped[u.names[0]] {
			dirty[u] = true
		}
	}
	for again := true; again; {
		again = false
		for _, u := range all {
			if !dirty[u] {
				for r := range refs[u] {
					if changedNames[r] {
						dirty[u], again = true, true
						break
					}
				}
			}
			if !dirty[u] || u.kind != cellsrc.Func || u.key[:min(4, len(u.key))] == "init" && u.names == nil && u.method == "" {
				continue
			}
			sig := s.sigOf(p, decls[u], lo)
			if sigs[u] == sig {
				continue
			}
			sigs[u] = sig
			again = true
			o := s.units[u.key]
			if u.method != "" {
				// Yaegi cannot replace a method's signature (or a generic
				// method) in place: make a new version of the type.
				if o != nil && !bumped[u.recv] && o.recvInternal == lo.name(u.recv) && (o.sig != sig || u.generic || o.generic) {
					force[u.recv] = true
					return true, nil
				}
				continue
			}
			if o == nil || o.sig != sig || o.generic || u.generic || o.broken != nil && o.storage == "" {
				if n := u.names[0]; names[n] == "" {
					names[n] = s.fresh(n)
				}
				changedNames[u.names[0]] = true
			}
		}
	}

	// Generate Yaegi source.
	// Types, variables and methods are evaluated in turn: together they trip
	// over Yaegi's initialization order analysis.
	var tdecl, vdecl, methods, inits strings.Builder
	storage := map[*unit]string{}
	for _, u := range all {
		if !dirty[u] {
			continue
		}
		d := decls[u]
		pos := p.fset.Position(d.Pos())
		where := s.lineMark(u.cell, pos.Line, pos.Column)
		switch u.kind {
		case cellsrc.Type, cellsrc.Const:
			lo.walk(d)
			tdecl.WriteString(where + lo.text(p.offset(d.Pos()), p.offset(d.End())) + "\n")
		case cellsrc.Func:
			fd := d.(*ast.FuncDecl)
			lo.walk(fd)
			if u.generic {
				methods.WriteString(where + lo.text(p.offset(d.Pos()), p.offset(d.End())) + "\n")
				if u.method != "" {
					u.recvInternal = lo.name(u.recv)
				}
				break
			}
			if u.names == nil && u.method == "" { // func init
				inits.WriteString("func() " + where + lo.text(p.offset(fd.Body.Pos()), p.offset(fd.Body.End())) + "()\n")
				break
			}
			o := s.units[u.key]
			var st string
			if u.method != "" {
				if o != nil && o.storage != "" && !bumped[u.recv] && o.recvInternal == lo.name(u.recv) {
					st = o.storage
				} else {
					st = s.fresh(u.recv + mark + u.method)
					lo.global(&vdecl, st, lo.implType(fd))
					methods.WriteString(lo.forwarder(fd, u, st))
				}
			} else if st = lo.name(u.names[0]); o == nil || o.storage != st {
				lo.global(&vdecl, st, lo.implType(fd))
			}
			storage[u] = st
			if u.method != "" {
				u.recvInternal = lo.name(u.recv)
			}
			fmt.Fprintf(&inits, "%s = %s%s%s\n", st, where, lo.funcType(fd), lo.text(p.offset(fd.Type.End()), p.offset(fd.Body.End())))
		}
	}
	for _, nv := range vars {
		if !nv.keep {
			lo.global(&vdecl, nv.v.internal, lo.typ(nv.v.typ))
		}
	}
	s.varInits(p, lo, newVars, &inits)
	if lo.err != nil {
		return false, &proto.CellError{Kind: proto.ErrInterp, Message: "not supported interactively: " + lo.err.Error()}
	}
	if e := s.importYaegi(); e != nil {
		return false, e
	}
	run := ""
	if inits.Len() > 0 {
		run = s.fresh("decl" + mark)
		fmt.Fprintf(&methods, "func %s() {\n%s}\n", run, inits.String())
	}
	for _, src := range []string{tdecl.String() + lo.aliases, vdecl.String(), methods.String()} {
		if src == "" {
			continue
		}
		if err := s.eval(src); err != nil {
			return false, s.interpError(err)
		}
	}

	// Commit.
	for _, n := range sortedKeys(bumped) {
		s.commitType(p, n, names[n], cell, fresh)
	}
	for n, v := range names {
		s.names[n] = v
	}
	for k, u := range s.units {
		clash := false
		for _, n := range u.names {
			clash = clash || freshNames[n]
		}
		for _, n := range newVars {
			clash = clash || len(u.names) == 1 && u.names[0] == n
		}
		if clash || strings.HasPrefix(k, "init"+mark) {
			delete(s.units, k)
		}
	}
	for _, u := range all {
		if isFresh[u] {
			s.seq++
			u.seq = s.seq
			u.text = s.canonicalText(p, decls[u])
		}
		u.refs = refs[u]
		u.broken = nil
		if sig, ok := sigs[u]; ok {
			u.sig = sig
		}
		if st, ok := storage[u]; ok {
			u.storage = st
		}
		s.units[u.key] = u
	}
	for n := range excluded {
		s.vars[n].stale = true
	}
	for _, u := range old {
		if b := broke[u]; b != nil {
			wasBroken := u.broken != nil
			u.broken = b
			if edited := editedTypes(u, fresh); !wasBroken || len(edited) > 0 {
				s.rt.notice(s.brokenNotice(u, edited, broke))
			}
		}
	}
	for n := range freshNames {
		delete(s.vars, n)
	}
	for _, nv := range vars {
		s.commitVar(nv.v, p, nil)
	}
	if run != "" {
		if e := s.call(run, cell, nil); e != nil {
			return false, e
		}
	}
	return false, nil
}

// label is how messages call a unit.
func (u *unit) label() string {
	if u.method != "" {
		return u.recv + "." + u.method
	}
	return u.names[0]
}

// editedTypes lists the types declared by the running cell that u depends on.
func editedTypes(u *unit, fresh []*unit) []string {
	var out []string
	for _, f := range fresh {
		if f.kind == cellsrc.Type && (u.refs[f.names[0]] || u.recv == f.names[0]) {
			out = append(out, f.names[0])
		}
	}
	return out
}

// brokenNotice says that a declaration could not follow a change, and why.
func (s *Session) brokenNotice(u *unit, edited []string, broke map[*unit]*broken) string {
	msg := fmt.Sprintf("%s (%s) could not update", u.label(), s.cellRef(u.cell))
	for i, n := range edited {
		sep := ", "
		if i == 0 {
			sep = " for "
		}
		if ti := s.types[n]; ti != nil {
			msg += fmt.Sprintf("%s%s v%d", sep, n, ti.version())
		}
	}
	// Something it uses may be to blame: then there is nothing to edit in it.
	for _, n := range u.broken.names {
		if v := s.vars[n]; v != nil && v.stale {
			return fmt.Sprintf("%s: it uses stale variable %s — re-run %s", msg, n, s.cellRef(v.cell))
		}
	}
	for o := range broke {
		if o == u {
			continue
		}
		uses := o.method != "" && missingMethod(u.broken.msg, o)
		for _, n := range u.broken.names {
			for _, x := range o.names {
				uses = uses || x == n
			}
		}
		if uses {
			return fmt.Sprintf("%s: it uses %s (%s), which did not update either", msg, o.label(), s.cellRef(o.cell))
		}
	}
	return fmt.Sprintf("%s: %s — edit and re-run %s", msg, u.broken.msg, s.cellRef(u.cell))
}

// missingMethod reports whether a type error complains that method u is
// missing.
func missingMethod(msg string, u *unit) bool {
	m := regexp.MustCompile(`\bmethod (\w+)`).FindStringSubmatch(msg)
	return m != nil && m[1] == u.method && regexp.MustCompile(`\b`+regexp.QuoteMeta(u.recv)+`\b`).MatchString(msg)
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unitDecls maps every unit of a program to its declaration.
func (s *Session) unitDecls(p *program, out map[*unit]ast.Decl) {
	for _, d := range p.file.Decls {
		if pc := p.pieceAt(p.offset(d.Pos())); pc != nil && pc.unit != nil {
			out[pc.unit] = d
		}
	}
}

// refsOf returns the package-level names a declaration depends on.
func (s *Session) refsOf(p *program, n ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := p.info.Uses[id]
		if obj == nil {
			return true
		}
		if obj.Pkg() == p.pkg && obj.Parent() == p.pkg.Scope() {
			out[obj.Name()] = true
		}
		// A value's type matters even if it is not named: u.Name depends on
		// the type of u.
		switch obj.(type) {
		case *types.Var, *types.Func:
			mainTypes(obj.Type(), out)
		}
		return true
	})
	return out
}

// sigOf renders a function's type in terms of internal names.
func (s *Session) sigOf(p *program, d ast.Decl, lo *lowerer) string {
	fd := d.(*ast.FuncDecl)
	fn, ok := p.info.Defs[fd.Name].(*types.Func)
	if !ok {
		return ""
	}
	sig := fn.Type().(*types.Signature)
	var b strings.Builder
	if sig.Recv() != nil {
		if pt, ok := sig.Recv().Type().(*types.Pointer); ok {
			_ = pt
			b.WriteString("*")
		}
	}
	defer func() { _ = recover() }() // unexported foreign types: keep what we have
	writeSignature(&b, sig, lo.typeName)
	return b.String()
}

// funcType renders "func(params) results", with the receiver as first
// parameter, keeping parameter names.
func (l *lowerer) funcType(fd *ast.FuncDecl) string {
	var ps []string
	list := func(f *ast.Field, name string) {
		t := l.text(l.p.offset(f.Type.Pos()), l.p.offset(f.Type.End()))
		ps = append(ps, name+" "+t)
	}
	if fd.Recv != nil {
		f := fd.Recv.List[0]
		n := "_"
		if len(f.Names) == 1 {
			n = f.Names[0].Name
		}
		list(f, n)
	}
	for _, f := range fd.Type.Params.List {
		if len(f.Names) == 0 {
			list(f, "_")
		}
		for _, n := range f.Names {
			list(f, n.Name)
		}
	}
	out := "func(" + strings.Join(ps, ", ") + ")"
	if fd.Type.Results != nil {
		out += " " + l.text(l.p.offset(fd.Type.Results.Pos()), l.p.offset(fd.Type.Results.End()))
	}
	return out
}

// implType is funcType without parameter names.
func (l *lowerer) implType(fd *ast.FuncDecl) string {
	sig := l.p.info.Defs[fd.Name].Type().(*types.Signature)
	if sig.Recv() != nil {
		ps := []*types.Var{types.NewParam(0, nil, "", sig.Recv().Type())}
		for i := 0; i < sig.Params().Len(); i++ {
			ps = append(ps, sig.Params().At(i))
		}
		sig = types.NewSignatureType(nil, nil, nil, types.NewTuple(ps...), sig.Results(), sig.Variadic())
	}
	return l.typ(sig)
}

// global declares a variable for Yaegi. Yaegi mistakes any identifier inside
// the declaration, such as a field name, for a use of the global variable of
// that name, so types that contain such names are declared on their own.
func (l *lowerer) global(b *strings.Builder, name, typ string) {
	if strings.Contains(typ, "struct{") || strings.Contains(typ, "interface{") && !strings.Contains(typ, "interface{}") {
		t := l.s.fresh("type" + mark)
		l.aliases += fmt.Sprintf("type %s = %s\n", t, typ)
		typ = t
	}
	fmt.Fprintf(b, "var %s %s\n", name, typ)
}

// forwarder declares a method that calls the function stored in variable st,
// so that the method body can be replaced later.
func (l *lowerer) forwarder(fd *ast.FuncDecl, u *unit, st string) string {
	sig := l.p.info.Defs[fd.Name].Type().(*types.Signature)
	var b strings.Builder
	rt := fd.Recv.List[0].Type
	fmt.Fprintf(&b, "func (r%s %s) %s(", mark, l.text(l.p.offset(rt.Pos()), l.p.offset(rt.End())), u.method)
	args := "r" + mark
	for i := 0; i < sig.Params().Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "p%d ", i)
		writeParam(&b, sig, i, l.typeName)
		args += fmt.Sprintf(", p%d", i)
	}
	if sig.Variadic() {
		args += "..."
	}
	b.WriteString(")")
	writeResults(&b, sig, l.typeName)
	ret := ""
	if sig.Results().Len() > 0 {
		ret = "return "
	}
	fmt.Fprintf(&b, " { %s%s(%s) }\n", ret, st, args)
	return b.String()
}

// canonicalText returns a declaration with imports spelled canonically, so
// that it stays valid whatever names later cells import packages under.
func (s *Session) canonicalText(p *program, d ast.Decl) string {
	l := &lowerer{s: s, p: p}
	ast.Inspect(d, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if pn, ok := p.info.Uses[id].(*types.PkgName); ok {
				l.replace(p.offset(id.Pos()), p.offset(id.End()), s.alias(pn.Imported().Path()))
			}
		}
		return true
	})
	return l.text(p.offset(d.Pos()), p.offset(d.End()))
}

// pkgVarNames lists the variables declared by package-level var chunks.
func (s *Session) pkgVarNames(cell string, chunks []cellsrc.Chunk) (names []string, e *proto.CellError) {
	for _, c := range chunks {
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p;"+c.Text, parser.SkipObjectResolution)
		if err != nil {
			return nil, syntaxError(err)
		}
		for _, d := range f.Decls {
			for _, sp := range d.(*ast.GenDecl).Specs {
				for _, id := range sp.(*ast.ValueSpec).Names {
					if id.Name != "_" {
						names = append(names, id.Name)
					}
				}
			}
		}
	}
	return names, nil
}

// varInits emits the initialization of package-level variables in Go's
// initialization order.
func (s *Session) varInits(p *program, lo *lowerer, newVars []string, out *strings.Builder) {
	isNew := map[string]bool{}
	for _, n := range newVars {
		isNew[n] = true
	}
	done := map[string]bool{}
	var body strings.Builder
	for _, in := range p.info.InitOrder {
		var lhs []string
		mine := false
		for _, v := range in.Lhs {
			if v.Name() == "_" {
				lhs = append(lhs, "_")
			} else {
				lhs = append(lhs, lo.name(v.Name()))
				mine = mine || isNew[v.Name()]
				done[v.Name()] = true
			}
		}
		if pc := p.pieceAt(p.offset(in.Rhs.Pos())); !mine && (pc == nil || !pc.new) {
			continue
		}
		lo.walk(in.Rhs)
		pos := p.fset.Position(in.Rhs.Pos())
		fmt.Fprintf(&body, "%s = %s%s\n", strings.Join(lhs, ", "), s.lineMark(pos.Filename, pos.Line, pos.Column),
			lo.text(p.offset(in.Rhs.Pos()), p.offset(in.Rhs.End())))
	}
	for _, n := range newVars {
		if !done[n] {
			if obj := p.pkg.Scope().Lookup(n); obj != nil {
				fmt.Fprintf(out, "{ var z%s %s; %s = z%s }\n", mark, lo.typ(obj.Type()), lo.name(n), mark)
			}
		}
	}
	out.WriteString(body.String())
}

func (s *Session) commitType(p *program, name, internal, cell string, fresh []*unit) {
	obj, ok := p.pkg.Scope().Lookup(name).(*types.TypeName)
	if !ok {
		delete(s.types, name)
		return
	}
	ti := s.types[name]
	if ti == nil {
		ti = &typeInfo{name: name, cell: cell}
		s.types[name] = ti
	}
	for _, u := range fresh {
		if u.kind == cellsrc.Type && u.names[0] == name {
			ti.cell = u.cell
		}
	}
	sc := s.schemaOf(obj.Type())
	sc.internal = internal
	s.rt.named["main."+internal] = obj.Type()
	ti.internal = internal
	ti.versions = append(ti.versions, sc)
	if n := len(ti.versions); n > 1 {
		ch := proto.Change{Type: name, From: n - 1, To: n, Details: diffSchema(ti.versions[n-2], sc)}
		ti.history = append(ti.history, ch)
		s.rt.notice(fmt.Sprintf("%s v%d -> v%d: %s", name, ch.From, ch.To, strings.Join(ch.Details, ", ")))
	}
}

// commitVar records a variable after its declaration ran.
func (s *Session) commitVar(v *variable, p *program, captures map[string]bool) {
	s.seq++
	v.seq = s.seq
	if o := s.vars[v.name]; o != nil {
		v.seq = o.seq
	}
	refs := map[string]bool{}
	mainTypes(v.typ, refs)
	for c := range captures {
		refs[c] = true
	}
	for again := true; again; { // types inside its types
		again = false
		for r := range refs {
			if u := s.units[r]; u != nil && u.kind == cellsrc.Type {
				for x := range u.refs {
					if !refs[x] && s.types[x] != nil {
						refs[x], again = true, true
					}
				}
			}
		}
	}
	v.captures = captures
	v.versions = map[string]int{}
	for n := range refs {
		if ti := s.types[n]; ti != nil {
			v.versions[n] = ti.version()
		}
	}
	s.vars[v.name] = v
	s.names[v.name] = v.internal
	// A variable hides a declaration of the same name.
	for k, u := range s.units {
		if len(u.names) == 1 && u.names[0] == v.name {
			delete(s.units, k)
		}
	}
	delete(s.types, v.name)
}
