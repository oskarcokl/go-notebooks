package cellsrc

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	src := "import \"fmt\"; x := 1\nfor i := 0; i < 3; i++ { x++ }; if y := x; y > 0 {\n} else if z := 2; z > 1 { }\n" +
		"func f() {}\nfunc (t T) m() {}\nfunc() { x-- }()\nswitch q := x; q {\ncase 1:\n}\n" +
		"if g := func() bool { return true }; g() {\n}\nvar (\n\ta = 1\n)\ntype T struct{}\nconst c = `a;\n}`\nL:\nfor {\nbreak L\n}"
	chunks, err := Split(src)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []Kind
	for _, c := range chunks {
		kinds = append(kinds, c.Kind)
	}
	want := []Kind{Import, Stmt, Stmt, Stmt, Func, Func, Stmt, Stmt, Stmt, Var, Type, Const, Stmt}
	if !reflect.DeepEqual(kinds, want) {
		for _, c := range chunks {
			t.Logf("%s %q", c.Kind, c.Text)
		}
		t.Errorf("got %v\nwant %v", kinds, want)
	}
	if c := chunks[2]; c.Line != 2 || c.Col != 1 || c.Text != "for i := 0; i < 3; i++ { x++ }" {
		t.Errorf("%+v", c)
	}
}

func TestSplitErrors(t *testing.T) {
	for _, src := range []string{"x := (1", "}", "s := \"abc", "`abc"} {
		if _, err := Split(src); err == nil {
			t.Errorf("%q: no error", src)
		}
	}
}

func TestImports(t *testing.T) {
	got := Imports("import (\n\t\"fmt\"\n\t_ \"a.b/c\"\n)\nfmt.Println()\nimport x \"os\"")
	if !reflect.DeepEqual(got, []string{"fmt", "a.b/c", "os"}) {
		t.Errorf("%q", got)
	}
}
