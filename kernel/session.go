// Package kernel runs notebook cells in one live Yaegi interpreter.
//
// Every cell is first checked with go/types as part of a plain Go program made
// of all live declarations and variables. This yields ordinary Go error
// messages, the exact type of every variable, and the dependencies between
// declarations, before the interpreter sees any code. The cell is then handed
// to Yaegi with a few source edits (see lower.go): notebook variables become
// package-level variables, and redefined names get a fresh internal name, so
// that old values keep their old types.
package kernel

import (
	"bytes"
	"fmt"
	"go/types"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/traefik/yaegi/interp"

	"go-notebooks/kernel/cellsrc"
	"go-notebooks/kernel/proto"
)

const (
	mainPath = "main"
	mark     = "ᐟ" // separates a name from its internal version; a letter no one types
)

// unit is one package-level declaration: a type, a const group, a func or a
// method.
type unit struct {
	key          string
	kind         cellsrc.Kind
	names        []string // declared package-level names
	recv, method string   // for methods
	generic      bool
	raw          string // as typed
	text         string // with imports spelled canonically
	cell         string
	line, col    int
	seq          int

	refs         map[string]bool // package-level names it mentions
	sig          string          // funcs: signature in terms of internal names
	storage      string          // funcs: variable holding the implementation
	recvInternal string          // methods: internal type the method was added to
	broken       *broken         // set while it does not compile against the current state
}

type broken struct {
	msg   string
	names []string // undefined names that caused it
}

// variable is a live notebook variable.
type variable struct {
	name     string
	internal string
	typ      types.Type
	typeStr  string // type as source, imports spelled canonically
	cell     string
	seq      int
	versions map[string]int  // versions of the notebook types it was built from
	captures map[string]bool // notebook types used inside closures it holds
	stale    bool
}

// typeInfo tracks the versions of a named type.
type typeInfo struct {
	name     string
	cell     string
	internal string
	versions []schema // versions[i] is v(i+1)
	history  []proto.Change
}

type schema struct {
	text     string
	isStruct bool
	fields   []proto.Field
	internal string
}

func (t *typeInfo) version() int { return len(t.versions) }

// Session is the state of one notebook.
type Session struct {
	mu       sync.Mutex
	in       *interp.Interpreter
	imp      types.Importer
	manifest *proto.Manifest
	symbols  map[string]bool // import paths Yaegi has symbols for
	trace    *traceWriter

	imports  map[string]string // user's import name -> path
	aliases  map[string]string // path -> canonical alias
	inYaegi  map[string]bool   // paths imported in Yaegi
	units    map[string]*unit
	names    map[string]string // package-level name -> internal name
	vars     map[string]*variable
	types    map[string]*typeInfo
	counters map[string]int
	seq      int
	cellNums map[string]int
	cells    map[string]int // numbering sent with the last request

	rt *runtimeState
}

// NewSession creates a session from the symbol tables linked into the kernel.
func NewSession(man *proto.Manifest, stdout, stderr io.Writer, tables ...interp.Exports) (*Session, error) {
	s := &Session{
		manifest: man,
		symbols:  map[string]bool{},
		imports:  map[string]string{},
		aliases:  map[string]string{},
		inYaegi:  map[string]bool{},
		units:    map[string]*unit{},
		names:    map[string]string{},
		vars:     map[string]*variable{},
		types:    map[string]*typeInfo{},
		counters: map[string]int{},
		cellNums: map[string]int{},
		trace:    &traceWriter{w: stderr},
		rt:       &runtimeState{defers: map[string][]func(){}, named: map[string]types.Type{}},
	}
	s.in = interp.New(interp.Options{
		Stdout: stdout, Stderr: s.trace, Env: os.Environ(), Unrestricted: true,
	})
	tables = append(tables, s.rt.exports())
	for _, t := range tables {
		if err := s.in.Use(t); err != nil {
			return nil, err
		}
		for k := range t {
			if i := strings.LastIndex(k, "/"); i > 0 {
				s.symbols[k[:i]] = true
			}
		}
	}
	s.imp = newImporter(man)
	if _, err := s.in.Eval("package main\nimport " + rtAlias + " \"" + rtPath + "\"\n"); err != nil {
		return nil, err
	}
	return s, nil
}

// Exec runs one cell.
func (s *Session) Exec(cell, src string, cells map[string]int) (resp proto.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cells = cells
	s.rt.begin(cell)
	defer func() {
		if r := recover(); r != nil { // a bug in go-notebooks or Yaegi; the session survives
			resp.Error = &proto.CellError{Kind: proto.ErrInterp, Message: fmt.Sprintf("internal error: %v", r)}
		}
		resp.Result = s.rt.result
		resp.Notices = append(resp.Notices, s.rt.notices...)
	}()
	s.rt.runDefers(cell)
	if err := s.exec(cell, src); err != nil {
		resp.Error = err
	}
	s.scanDynamic()
	return resp
}

func (s *Session) exec(cell, src string) *proto.CellError {
	chunks, err := cellsrc.Split(src)
	if err != nil {
		return syntaxError(err)
	}
	hasStmt := false
	for _, c := range chunks {
		hasStmt = hasStmt || c.Kind == cellsrc.Stmt
	}
	var decls, stmts []cellsrc.Chunk
	flush := func() *proto.CellError {
		var e *proto.CellError
		if len(decls) > 0 {
			e = s.execDecls(cell, decls)
		} else if len(stmts) > 0 {
			e = s.execStmts(cell, stmts)
		}
		decls, stmts = nil, nil
		return e
	}
	for _, c := range chunks {
		isStmt := c.Kind == cellsrc.Stmt || c.Kind == cellsrc.Var && hasStmt
		if isStmt && len(decls) > 0 || !isStmt && len(stmts) > 0 {
			if e := flush(); e != nil {
				return e
			}
		}
		if isStmt {
			stmts = append(stmts, c)
		} else {
			decls = append(decls, c)
		}
	}
	return flush()
}

// Yaegi resolves imports by file name, so line marks cannot name the cell:
// every cell gets its own range of line numbers instead.
const cellLines = 1 << 16

func (s *Session) lineMark(cell string, line, col int) string {
	n, ok := s.cellNums[cell]
	if !ok {
		n = len(s.cellNums) + 1
		s.cellNums[cell] = n
	}
	return fmt.Sprintf("/*line %s:%d:%d*/", interp.DefaultSourceName, n*cellLines+line, col)
}

func (s *Session) linePos(n int) tracePos {
	for cell, i := range s.cellNums {
		if i == n/cellLines {
			return tracePos{cell, n % cellLines}
		}
	}
	return tracePos{}
}

var posRE = regexp.MustCompile(`^(?:[^\s:]*:)?(\d+):\d+: `)

func syntaxError(err error) *proto.CellError {
	e := &proto.CellError{Kind: proto.ErrSyntax, Message: "syntax error: " + err.Error()}
	if m := posRE.FindStringSubmatch(err.Error()); m != nil {
		fmt.Sscan(m[1], &e.Line)
		e.Message = "syntax error: " + err.Error()[len(m[0]):]
	}
	return e
}

// fresh returns an unused internal name for a user-visible one. The first use
// of a name keeps it as is, except for names special to Yaegi.
func (s *Session) fresh(name string) string {
	s.counters[name]++
	if n := s.counters[name]; n > 1 || name == "main" || name == "init" {
		return fmt.Sprintf("%s%s%d", name, mark, n)
	}
	return name
}

var markRE = regexp.MustCompile(mark + `\d*`)

// demangle turns internal names in messages back into what the user wrote.
func (s *Session) demangle(msg string) string {
	for path, a := range s.aliases {
		if strings.Contains(msg, a) {
			msg = strings.ReplaceAll(msg, a, s.pkgName(path))
		}
	}
	return markRE.ReplaceAllString(msg, "")
}

func (s *Session) pkgName(path string) string {
	if n := s.manifest.Names[path]; n != "" {
		return n
	}
	return path[strings.LastIndex(path, "/")+1:]
}

// alias returns the canonical package identifier for an import path.
func (s *Session) alias(path string) string {
	if a, ok := s.aliases[path]; ok {
		return a
	}
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, path)
	a := fmt.Sprintf("pkg%s%d%s", mark, len(s.aliases), "_"+clean)
	s.aliases[path] = a
	return a
}

// canonical spells types for storage: notebook types by their own name,
// imported ones through the canonical alias.
func (s *Session) canonical(o *types.TypeName) string {
	if o.Pkg() == nil || o.Pkg().Path() == mainPath {
		return o.Name()
	}
	return s.alias(o.Pkg().Path()) + "." + o.Name()
}

// internalName spells types for Yaegi.
func (s *Session) internalName(o *types.TypeName) string {
	if o.Pkg() != nil && o.Pkg().Path() == mainPath && o.Parent() == o.Pkg().Scope() {
		if n, ok := s.names[o.Name()]; ok {
			return n
		}
		return o.Name()
	}
	return s.canonical(o)
}

// display spells types for people.
func (s *Session) display(o *types.TypeName) string {
	if o.Pkg() == nil || o.Pkg().Path() == mainPath {
		return o.Name()
	}
	return o.Pkg().Name() + "." + o.Name()
}

func (s *Session) displayType(t types.Type) string {
	str, err := typeString(t, s.display)
	if err != nil {
		return t.String()
	}
	return strings.ReplaceAll(str, "interface{}", "any")
}

// debug makes the kernel print the code it gives to Yaegi.
var debug = os.Getenv("GONOTEBOOKS_DEBUG") != ""

// eval hands code to Yaegi, turning its panics into errors.
func (s *Session) eval(src string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	if debug {
		fmt.Fprintf(os.Stderr, "--- yaegi\n%s\n", src)
	}
	_, err = s.in.Eval("package main\n" + src)
	return err
}

// importYaegi makes all packages with a canonical alias known to Yaegi.
func (s *Session) importYaegi() *proto.CellError {
	var b strings.Builder
	var paths []string
	for p := range s.aliases {
		if !s.inYaegi[p] && s.symbols[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&b, "import %s %q\n", s.aliases[p], p)
	}
	if err := s.eval(b.String()); err != nil {
		return s.interpError(err)
	}
	for _, p := range paths {
		s.inYaegi[p] = true
	}
	return nil
}

func (s *Session) interpError(err error) *proto.CellError {
	msg := s.demangle(err.Error())
	e := &proto.CellError{Kind: proto.ErrInterp}
	if m := posRE.FindStringSubmatch(msg); m != nil {
		fmt.Sscan(m[1], &e.Line)
		e.Line = s.linePos(e.Line).line
		msg = msg[len(m[0]):]
	}
	e.Message = "this is valid Go, but the interpreter does not support it: " + msg
	return e
}

// traceWriter separates Yaegi's panic trace lines from ordinary stderr output.
type traceWriter struct {
	w     io.Writer
	mu    sync.Mutex
	lines []int
}

type tracePos struct {
	cell string
	line int
}

var traceRE = regexp.MustCompile(`^(?:_\.go:)?(\d+):\d+: panic: main\b`)

func (t *traceWriter) Write(p []byte) (int, error) {
	if m := traceRE.FindSubmatch(p); m != nil && bytes.Count(p, []byte("\n")) <= 1 {
		line := 0
		fmt.Sscan(string(m[1]), &line)
		t.mu.Lock()
		t.lines = append(t.lines, line)
		t.mu.Unlock()
		return len(p), nil
	}
	return t.w.Write(p)
}

func (t *traceWriter) take() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.lines
	t.lines = nil
	return l
}

func sortedUnits(m map[string]*unit) []*unit {
	out := make([]*unit, 0, len(m))
	for _, u := range m {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

func (s *Session) sortedVars() []*variable {
	out := make([]*variable, 0, len(s.vars))
	for _, v := range s.vars {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}
