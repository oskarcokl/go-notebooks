// Package notebook reads and writes notebooks stored as ordinary Go files.
//
// Cells are separated by percent-style comment markers:
//
//	// %% [markdown]
//	// # Some *markdown*
//
//	// %% Optional title
//	type User struct{ Name string }
//
//	func main() {
//		// %%
//		u := User{Name: "Ada"}
//	}
//
// Cells before and after func main hold package-level declarations; cells
// inside its body hold statements.
package notebook

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kinds of cells.
const (
	Markdown = "markdown"
	Code     = "code"
)

// Section says where in the file a cell lives.
type Section int

// Sections in file order.
const (
	Head Section = iota // before the package clause (markdown only)
	Top                 // package level, before func main
	Main                // inside func main
	Post                // package level, after func main
)

// String returns the name used by the JSON API.
func (s Section) String() string { return [...]string{"head", "package", "main", "post"}[s] }

// Cell is one notebook cell.
type Cell struct {
	ID      string
	Kind    string
	Title   string
	Source  string
	Section Section

	implicit bool // written without a marker, as it was read
}

// Notebook is a parsed file.
type Notebook struct {
	Cells []*Cell

	preamble string // package clause with its doc comment and build tags
	hasMain  bool
	nextID   int
}

// New returns an empty package main notebook.
func New() *Notebook { return &Notebook{preamble: "package main", hasMain: true} }

var (
	markerRE  = regexp.MustCompile(`^[ \t]*//[ \t]*%%(?:[ \t]+(.*))?$`)
	packageRE = regexp.MustCompile(`(?m)^package[ \t]+(\w+)`)
)

func (n *Notebook) newID() string {
	n.nextID++
	return fmt.Sprintf("c%d", n.nextID)
}

// Find returns the cell with the given id and its index, or nil and -1.
func (n *Notebook) Find(id string) (*Cell, int) {
	for i, c := range n.Cells {
		if c.ID == id {
			return c, i
		}
	}
	return nil, -1
}

// Package returns the package name.
func (n *Notebook) Package() string {
	if m := packageRE.FindAllStringSubmatch(n.preamble, -1); m != nil {
		return m[len(m)-1][1]
	}
	return "main"
}

// Load reads and parses a notebook file.
func Load(path string) (*Notebook, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}

// Save writes the notebook to path atomically.
func (n *Notebook) Save(path string) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gonb-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(n.Serialize())
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Parse reads notebook source. Cells with Go syntax errors are kept as they
// are; only a missing package clause is an error.
func Parse(src string) (*Notebook, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	n := &Notebook{}

	pkgEnd, mainStart, bodyStart, bodyEnd, mainEnd := layout(src)
	if pkgEnd < 0 {
		return nil, fmt.Errorf("not a Go file: no package clause found")
	}
	n.parseHead(src[:pkgEnd])
	if mainStart < 0 {
		n.parseCells(src[pkgEnd:], Top)
		return n, nil
	}
	n.hasMain = true
	n.parseCells(src[pkgEnd:mainStart], Top)
	// "// %% Title" directly above func main names the first cell of its body.
	var title *Cell
	if last := len(n.Cells) - 1; last >= 0 {
		if c := n.Cells[last]; c.Kind == Code && c.Section == Top && c.Source == "" && !c.implicit {
			title, n.Cells = c, n.Cells[:last]
		}
	}
	first := len(n.Cells)
	n.parseCells(dedent(src[bodyStart:bodyEnd]), Main)
	if title != nil && len(n.Cells) > first && n.Cells[first].implicit {
		n.Cells[first].Title, n.Cells[first].implicit = title.Title, false
	}
	n.parseCells(src[mainEnd:], Post)
	return n, nil
}

// layout finds the end of the package clause and the extent of func main and
// of its body. mainStart is -1 if there is no func main.
func layout(src string) (pkgEnd, mainStart, bodyStart, bodyEnd, mainEnd int) {
	mainStart = -1
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err == nil {
		off := func(p token.Pos) int { return fset.Position(p).Offset }
		pkgEnd = off(f.Name.End())
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" && fd.Body != nil {
				return pkgEnd, off(fd.Pos()), off(fd.Body.Lbrace) + 1, off(fd.Body.Rbrace), off(fd.Body.Rbrace) + 1
			}
		}
		return
	}
	// The file does not parse: rely on gofmt's layout, where the braces of a
	// top-level function are the only ones in column 0.
	m := packageRE.FindStringIndex(src)
	if m == nil {
		return -1, -1, 0, 0, 0
	}
	pkgEnd = m[1]
	raw := rawLines(src)
	off := 0
	for i, line := range strings.SplitAfter(src, "\n") {
		text := strings.TrimRight(line, " \t\n")
		switch {
		case raw[i]:
		case mainStart < 0 && strings.HasPrefix(text, "func main() {"):
			mainStart, bodyStart = off, off+len("func main() {")
			bodyEnd, mainEnd = len(src), len(src)
		case mainStart >= 0 && bodyEnd == len(src) && text == "}":
			bodyEnd, mainEnd = off, off+1
		}
		off += len(line)
	}
	return
}

// rawLines reports which lines begin inside a raw string literal.
func rawLines(src string) map[int]bool {
	out := map[int]bool{}
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var sc scanner.Scanner
	sc.Init(file, []byte(src), func(token.Position, string) {}, 0)
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.STRING && strings.HasPrefix(lit, "`") && len(lit) > 1 && strings.HasSuffix(lit, "`") {
			line := file.Line(pos) - 1
			for k := range strings.Count(src[file.Offset(pos):min(len(src), file.Offset(pos)+len(lit))], "\n") {
				out[line+1+k] = true
			}
		}
	}
}

func (n *Notebook) parseHead(src string) {
	lines := strings.Split(src, "\n")
	var keep []string
	for i := 0; i < len(lines); {
		if kind, title, ok := marker(lines[i]); ok {
			i++
			var body []string
			for i < len(lines) && strings.HasPrefix(lines[i], "//") && !isMarker(lines[i]) {
				body = append(body, lines[i])
				i++
			}
			if kind == Markdown {
				n.add(&Cell{Kind: Markdown, Title: title, Source: uncomment(body), Section: Head})
			} else {
				keep = append(keep, body...)
			}
			continue
		}
		keep = append(keep, lines[i])
		i++
	}
	n.preamble = strings.TrimSpace(strings.Join(keep, "\n"))
}

func (n *Notebook) add(c *Cell) {
	c.ID = n.newID()
	n.Cells = append(n.Cells, c)
}

func isMarker(line string) bool { _, _, ok := marker(line); return ok }

// marker parses a "// %%" line.
func marker(line string) (kind, title string, ok bool) {
	m := markerRE.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	title = strings.TrimSpace(m[1])
	if rest, found := strings.CutPrefix(title, "[markdown]"); found {
		return Markdown, strings.TrimSpace(rest), true
	}
	if rest, found := strings.CutPrefix(title, "[md]"); found {
		return Markdown, strings.TrimSpace(rest), true
	}
	return Code, title, true
}

func (n *Notebook) parseCells(src string, sec Section) {
	raw := rawLines(src)
	lines := strings.Split(src, "\n")
	cur := &Cell{Kind: Code, Section: sec, implicit: true}
	var body []string
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Kind == Markdown {
			cur.Source = uncomment(body)
		} else {
			cur.Source = trimBlank(body)
			if cur.implicit && cur.Source == "" {
				body = nil
				return
			}
		}
		n.add(cur)
		body = nil
	}
	for i, line := range lines {
		if !raw[i] {
			if kind, title, ok := marker(line); ok {
				flush()
				cur = &Cell{Kind: kind, Title: title, Section: sec}
				continue
			}
		}
		// Markdown ends at the first line that is not a comment.
		if cur.Kind == Markdown && !strings.HasPrefix(strings.TrimSpace(line), "//") {
			flush()
			cur = &Cell{Kind: Code, Section: sec, implicit: true}
		}
		body = append(body, line)
	}
	flush()
}

func trimBlank(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}

func uncomment(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		l = strings.TrimPrefix(strings.TrimSpace(l), "//")
		out[i] = strings.TrimPrefix(l, " ")
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// dedent removes one tab from every line that is not inside a raw string.
func dedent(src string) string {
	return reindent(src, func(l string) string { return strings.TrimPrefix(l, "\t") })
}

func indent(src string) string {
	return reindent(src, func(l string) string {
		if strings.TrimSpace(l) == "" {
			return ""
		}
		return "\t" + l
	})
}

func reindent(src string, f func(string) string) string {
	raw := rawLines(src)
	lines := strings.Split(src, "\n")
	for i := range lines {
		if !raw[i] {
			lines[i] = f(lines[i])
		}
	}
	return strings.Join(lines, "\n")
}

// Serialize renders the notebook as a Go file, formatted if it parses.
func (n *Notebook) Serialize() string {
	var sec [4]strings.Builder
	var prev [4]*Cell
	for _, c := range n.Cells {
		b := &sec[c.Section]
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// An unmarked cell stays unmarked only where it cannot merge with the
		// cell above it.
		p := prev[c.Section]
		b.WriteString(c.render(c.implicit && c.Source != "" && (p == nil || p.Kind == Markdown)))
		prev[c.Section] = c
	}
	var out strings.Builder
	if sec[Head].Len() > 0 {
		out.WriteString(sec[Head].String() + "\n")
	}
	out.WriteString(n.preamble + "\n")
	if sec[Top].Len() > 0 {
		out.WriteString("\n" + sec[Top].String())
	}
	if n.hasMain || sec[Main].Len() > 0 || n.Package() == "main" {
		out.WriteString("\nfunc main() {\n")
		if sec[Main].Len() > 0 {
			out.WriteString(indent(strings.TrimRight(sec[Main].String(), "\n")) + "\n")
		}
		out.WriteString("}\n")
	}
	if sec[Post].Len() > 0 {
		out.WriteString("\n" + sec[Post].String())
	}
	if b, err := format.Source([]byte(out.String())); err == nil {
		return string(b)
	}
	return out.String()
}

func (c *Cell) render(unmarked bool) string {
	var b strings.Builder
	if c.Kind == Markdown {
		b.WriteString(strings.TrimRight("// %% [markdown] "+c.Title, " ") + "\n")
		for _, l := range strings.Split(strings.ReplaceAll(c.Source, "\r\n", "\n"), "\n") {
			l = strings.TrimRight(l, " \t")
			if isMarker("// " + l) {
				l = `\` + l // keep text that looks like a marker from splitting the cell
			}
			b.WriteString(strings.TrimRight("// "+l, " ") + "\n")
		}
		return b.String()
	}
	if !unmarked {
		b.WriteString(strings.TrimRight("// %% "+c.Title, " ") + "\n")
	}
	if s := formatCell(strings.TrimRight(strings.ReplaceAll(c.Source, "\r\n", "\n"), " \t\n")); s != "" {
		b.WriteString(s + "\n")
	}
	return b.String()
}

// formatCell gofmts one cell if it is valid on its own.
func formatCell(src string) string {
	if b, err := format.Source([]byte(src)); err == nil {
		return strings.TrimRight(string(b), "\n")
	}
	return src
}

// Refresh re-reads cell sources from the serialized form, so that cells show
// the text exactly as formatted on disk. Cell ids are kept.
func (n *Notebook) Refresh() {
	p, err := Parse(n.Serialize())
	if err != nil || len(p.Cells) != len(n.Cells) {
		return
	}
	for i, c := range p.Cells {
		if c.Kind != n.Cells[i].Kind || c.Section != n.Cells[i].Section {
			return
		}
	}
	for i, c := range p.Cells {
		o := n.Cells[i]
		o.Title, o.Source, o.implicit = c.Title, c.Source, c.implicit
	}
}

// Adopt replaces the contents of n with those of p, keeping the ids of cells
// whose kind and source did not change (or, failing that, of cells at the same
// place).
func (n *Notebook) Adopt(p *Notebook) {
	used := map[*Cell]bool{}
	match := func(ok func(i int, o, c *Cell) bool) {
		for i, c := range p.Cells {
			if strings.HasPrefix(c.ID, "old:") {
				continue
			}
			for _, o := range n.Cells {
				if !used[o] && ok(i, o, c) {
					used[o] = true
					c.ID = "old:" + o.ID
					break
				}
			}
		}
	}
	match(func(_ int, o, c *Cell) bool { return o.Kind == c.Kind && o.Source == c.Source })
	match(func(i int, o, c *Cell) bool { return i < len(n.Cells) && n.Cells[i] == o && o.Kind == c.Kind })
	for _, c := range p.Cells {
		if id, ok := strings.CutPrefix(c.ID, "old:"); ok {
			c.ID = id
		} else {
			c.ID = n.newID()
		}
	}
	n.Cells, n.preamble, n.hasMain = p.Cells, p.preamble, p.hasMain
}
