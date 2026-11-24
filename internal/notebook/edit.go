package notebook

import (
	"fmt"
	"strings"

	"go-notebooks/kernel/cellsrc"
)

// Insert adds a cell after the cell with id after ("" for the beginning).
func (n *Notebook) Insert(after, kind, title, source string) (*Cell, error) {
	if kind != Markdown && kind != Code {
		return nil, fmt.Errorf("unknown cell kind %q", kind)
	}
	i, sec := 0, Top
	if after != "" {
		prev, at := n.Find(after)
		if prev == nil {
			return nil, fmt.Errorf("no cell %q", after)
		}
		i, sec = at+1, prev.Section
		// New code after the last import/package cell is usually a statement
		// once the notebook has (or will have) a main body.
		if kind == Code && sec == Head {
			sec = Top
		}
	} else if len(n.Cells) > 0 {
		if sec = n.Cells[0].Section; sec == Head && kind == Code {
			sec = Top
		}
	}
	c := &Cell{ID: n.newID(), Kind: kind, Title: title, Source: source, Section: sec}
	n.Cells = append(n.Cells[:i], append([]*Cell{c}, n.Cells[i:]...)...)
	n.normalize()
	return c, nil
}

// Delete removes a cell.
func (n *Notebook) Delete(id string) error {
	_, i := n.Find(id)
	if i < 0 {
		return fmt.Errorf("no cell %q", id)
	}
	n.Cells = append(n.Cells[:i], n.Cells[i+1:]...)
	return nil
}

// Update changes a cell's kind, title or source; nil leaves a field alone.
func (n *Notebook) Update(id string, kind, title, source *string) error {
	c, _ := n.Find(id)
	if c == nil {
		return fmt.Errorf("no cell %q", id)
	}
	if kind != nil {
		if *kind != Markdown && *kind != Code {
			return fmt.Errorf("unknown cell kind %q", *kind)
		}
		c.Kind = *kind
	}
	if title != nil {
		c.Title = strings.Join(strings.Fields(*title), " ")
		c.implicit = c.implicit && c.Title == ""
	}
	if source != nil {
		c.Source = *source
	}
	n.normalize()
	return nil
}

// Move shifts a cell up (delta < 0) or down by one place. Moving across the
// edge of func main changes the cell's section when its contents allow that.
func (n *Notebook) Move(id string, delta int) error {
	c, i := n.Find(id)
	if c == nil {
		return fmt.Errorf("no cell %q", id)
	}
	j := i + 1
	if delta < 0 {
		j = i - 1
	}
	if j < 0 || j >= len(n.Cells) {
		// At either end only the section can still change.
		if c.Kind == Markdown && delta < 0 && c.Section > Top {
			c.Section = Top
		} else if c.Kind == Markdown && delta > 0 && c.Section < Main {
			c.Section = Main
		}
		return nil
	}
	if other := n.Cells[j]; other.Section != c.Section {
		c.Section = other.Section
		if c.Section == Head && c.Kind == Code {
			c.Section = Top
		}
	} else {
		n.Cells[i], n.Cells[j] = n.Cells[j], n.Cells[i]
	}
	n.normalize()
	return nil
}

// normalize gives every cell a section its contents are legal in and sorts
// cells by section, so that the file compiles.
func (n *Notebook) normalize() {
	for _, c := range n.Cells {
		if c.Kind != Code {
			continue
		}
		in, err := cellsrc.Classify(c.Source)
		if err != nil {
			continue
		}
		switch {
		case in.HasStmt && !in.HasFunc && !in.HasImport:
			if c.Section != Main && n.Package() == "main" {
				c.Section, c.implicit = Main, false
			}
		case (in.HasFunc || in.HasImport) && !in.HasStmt:
			if c.Section == Main || c.Section == Head {
				c.Section, c.implicit = Top, false
			}
		}
	}
	var by [4][]*Cell
	for _, c := range n.Cells {
		by[c.Section] = append(by[c.Section], c)
	}
	by[Top] = importsFirst(append(by[Top], takeImports(&by[Post])...))
	n.Cells = n.Cells[:0]
	for _, s := range by {
		n.Cells = append(n.Cells, s...)
	}
}

func isImport(c *Cell) bool {
	if c.Kind != Code {
		return false
	}
	in, err := cellsrc.Classify(c.Source)
	return err == nil && in.HasImport && !in.HasStmt
}

func takeImports(cells *[]*Cell) (out []*Cell) {
	var rest []*Cell
	for _, c := range *cells {
		if isImport(c) {
			c.Section = Top
			out = append(out, c)
		} else {
			rest = append(rest, c)
		}
	}
	*cells = rest
	return out
}

// importsFirst moves import cells above the first cell with other code, as Go
// requires. Markdown keeps its place.
func importsFirst(cells []*Cell) []*Cell {
	first := -1
	for i, c := range cells {
		if c.Kind == Code && strings.TrimSpace(c.Source) != "" && !isImport(c) {
			if in, err := cellsrc.Classify(c.Source); err == nil && (in.HasFunc || in.HasDecl) {
				first = i
				break
			}
		}
	}
	if first < 0 {
		return cells
	}
	var imports, rest []*Cell
	for _, c := range cells[first:] {
		if isImport(c) {
			imports = append(imports, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(append(append([]*Cell{}, cells[:first]...), imports...), rest...)
}

// Warning explains why a cell cannot be part of a compilable file, or is "".
func Warning(c *Cell) string {
	if c.Kind != Code {
		return ""
	}
	in, err := cellsrc.Classify(c.Source)
	if err != nil {
		return ""
	}
	if (in.HasFunc || in.HasImport) && in.HasStmt {
		return "this cell mixes statements with imports or func declarations: it runs here, but go build needs them in separate cells"
	}
	return ""
}

// Imports lists the import paths used by the notebook's cells.
func (n *Notebook) Imports() []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range n.Cells {
		if c.Kind != Code {
			continue
		}
		for _, p := range cellsrc.Imports(c.Source) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
