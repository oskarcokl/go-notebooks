package notebook

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `// %% [markdown]
// # Title
//
// Some *text*.

// Package main is a demo.
package main

// %%
import (
	"fmt"
)

// %% Types
type User struct {
	Name string
}

// %% [markdown]
// Functions

func (u User) Hi() string { return "hi " + u.Name }

func main() {
	// %% Create
	u := User{Name: "Ada"}

	// %% [markdown]
	// Inside main

	// %%
	s := ` + "`raw\nnot indented\n// %% not a marker\n}`" + `
	if u.Name != "" {
		fmt.Println(u.Hi(), s)
	}
}

// %% After
func helper() {}
`

func sources(n *Notebook) (out []string) {
	for _, c := range n.Cells {
		out = append(out, c.Section.String()+"|"+c.Kind+"|"+c.Title+"|"+c.Source)
	}
	return
}

func TestParse(t *testing.T) {
	n, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"head|markdown||# Title\n\nSome *text*.",
		"package|code||import (\n\t\"fmt\"\n)",
		"package|code|Types|type User struct {\n\tName string\n}",
		"package|markdown||Functions",
		"package|code||func (u User) Hi() string { return \"hi \" + u.Name }",
		"main|code|Create|u := User{Name: \"Ada\"}",
		"main|markdown||Inside main",
		"main|code||s := `raw\nnot indented\n// %% not a marker\n}`\nif u.Name != \"\" {\n\tfmt.Println(u.Hi(), s)\n}",
		"post|code|After|func helper() {}",
	}
	got := sources(n)
	if len(got) != len(want) {
		t.Fatalf("got %d cells:\n%s", len(got), strings.Join(got, "\n---\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	if p := n.Imports(); len(p) != 1 || p[0] != "fmt" {
		t.Errorf("imports = %v", p)
	}
}

func TestRoundTripIsByteIdentical(t *testing.T) {
	n, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Serialize(); got != sample {
		t.Errorf("round trip changed the file:\n%s", got)
	}
}

func TestRoundTripPlainGoFile(t *testing.T) {
	src := "//go:build ignore\n\npackage main\n\nimport \"fmt\"\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {\n\tfmt.Println(add(1, 2))\n}\n"
	n, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Cells) != 2 {
		t.Fatalf("cells: %q", sources(n))
	}
	if got := n.Serialize(); got != src {
		t.Errorf("got:\n%s", got)
	}
}

func TestMarkerAboveMainNamesFirstCell(t *testing.T) {
	n, err := Parse("package main\n\n// %% Run it\nfunc main() {\n\tprintln(1)\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Cells) != 1 || n.Cells[0].Title != "Run it" || n.Cells[0].Section != Main {
		t.Fatalf("%q", sources(n))
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse("# not go"); err == nil {
		t.Error("expected an error")
	}
}

func TestBrokenCellSurvives(t *testing.T) {
	n, _ := Parse(sample)
	c, _ := n.Find(n.Cells[5].ID)
	src := "u := User{Name: " // unfinished edit
	if err := n.Update(c.ID, nil, nil, &src); err != nil {
		t.Fatal(err)
	}
	n2, err := Parse(n.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	got, want := sources(n2), sources(n)
	for i := range want {
		if i >= len(got) || strings.TrimSpace(got[i]) != strings.TrimSpace(want[i]) {
			t.Errorf("cell %d: got %q want %q", i, got, want)
			break
		}
	}
}

func TestEditing(t *testing.T) {
	n, _ := Parse(sample)
	id := func(i int) string { return n.Cells[i].ID }

	// A statement inserted at package level moves into main.
	c, err := n.Insert(id(2), Code, "", "x := 1\n_ = x")
	if err != nil {
		t.Fatal(err)
	}
	if c.Section != Main {
		t.Errorf("statement cell in section %s", c.Section)
	}
	// A func written inside main moves out of it.
	f, _ := n.Insert(c.ID, Code, "", "func sq(n int) int { return n * n }")
	if f.Section != Top {
		t.Errorf("func cell in section %s", f.Section)
	}
	// Imports go above other declarations.
	im, _ := n.Insert(f.ID, Code, "", `import "strings"`)
	if _, i := n.Find(im.ID); i != 2 {
		t.Errorf("import cell at %d: %q", i, sources(n))
	}
	src := `import "os"; var _ = os.Args; var _ = strings.ToUpper`
	_ = src
	out := n.Serialize()
	if _, err := parser.ParseFile(token.NewFileSet(), "", out, 0); err != nil {
		t.Errorf("invalid Go after edits: %v\n%s", err, out)
	}

	before := len(n.Cells)
	if err := n.Delete(c.ID); err != nil || len(n.Cells) != before-1 {
		t.Errorf("delete: %v", err)
	}
	if err := n.Delete("nope"); err == nil {
		t.Error("deleting an unknown cell succeeded")
	}

	// Reorder two statement cells.
	var mains []string
	for _, c := range n.Cells {
		if c.Section == Main {
			mains = append(mains, c.ID)
		}
	}
	if err := n.Move(mains[0], 1); err != nil {
		t.Fatal(err)
	}
	if _, i0 := n.Find(mains[0]); i0 == 0 {
		t.Fatal("lost cell")
	}
	_, a := n.Find(mains[0])
	_, b := n.Find(mains[1])
	if a < b {
		t.Error("Move did not reorder")
	}
	// Round trip of the edited notebook.
	n2, err := Parse(n.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sources(n2), "\x00") != strings.Join(sources(n), "\x00") {
		t.Errorf("edited round trip:\n%q\n%q", sources(n2), sources(n))
	}
}

func TestImplicitCellsDoNotMerge(t *testing.T) {
	n, _ := Parse("package main\n\nvar a = 1\n\n// %% [markdown]\n// x\n\nvar b = 2\n\nfunc main() {\n}\n")
	if len(n.Cells) != 3 {
		t.Fatalf("%q", sources(n))
	}
	n.Delete(n.Cells[1].ID)
	n2, _ := Parse(n.Serialize())
	if len(n2.Cells) != 2 {
		t.Errorf("cells merged: %q", sources(n2))
	}
}

func TestMarkdownCannotSplitItself(t *testing.T) {
	n := New()
	n.Insert("", Markdown, "", "a\n%% b\n\nlast")
	n2, _ := Parse(n.Serialize())
	if len(n2.Cells) != 1 {
		t.Errorf("%q", sources(n2))
	}
}

func TestAdoptKeepsIDs(t *testing.T) {
	n, _ := Parse(sample)
	ids := map[string]string{}
	for _, c := range n.Cells {
		ids[c.Source] = c.ID
	}
	p, _ := Parse(strings.Replace(sample, "// %% [markdown]\n// Inside main\n\n", "", 1))
	n.Adopt(p)
	for _, c := range n.Cells {
		if ids[c.Source] != c.ID {
			t.Errorf("cell %q: id %s, want %s", c.Source, c.ID, ids[c.Source])
		}
	}
}

func TestSerializedNotebookBuildsAndRuns(t *testing.T) {
	n, _ := Parse(sample)
	src := "fmt.Println(helper2(4))"
	n.Insert(n.Cells[7].ID, Code, "", src)
	n.Insert(n.Cells[2].ID, Code, "", "func helper2(i int) int { return i * i }")
	dir := t.TempDir()
	file := filepath.Join(dir, "nb.go")
	if err := n.Save(file); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "nb")
	if out, err := exec.Command("go", "build", "-o", bin, file).CombinedOutput(); err != nil {
		b, _ := os.ReadFile(file)
		t.Fatalf("go build: %v\n%s\n%s", err, out, b)
	}
	out, err := exec.Command(bin).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "hi Ada") || !strings.HasSuffix(string(out), "16\n") {
		t.Errorf("run: %v\n%s", err, out)
	}
}
