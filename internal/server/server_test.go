package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-notebooks/internal/manager"
)

const source = `package main

// %% Imports
import "fmt"

// %% User
type User struct {
	Name string
}

func main() {
	// %% [markdown]
	// # Hello

	// %% Create
	u := User{Name: "Ada"}

	// %% Count
	n := 1

	// %% Show
	fmt.Println(u.Name, n)
}
`

type client struct {
	t    *testing.T
	srv  *httptest.Server
	path string
}

func start(t *testing.T, src string) *client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nb.go")
	if src != "" {
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e, err := manager.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(New(ctx, e))
	t.Cleanup(func() { cancel(); srv.Close(); e.Close() })
	return &client{t, srv, path}
}

func (c *client) do(method, path string, body, out any, want int) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: %s\n%s", method, path, resp.Status, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: %v\n%s", method, path, err, b)
		}
	}
}

func (c *client) view() (v manager.View) {
	c.t.Helper()
	c.do("GET", "/api/notebook", nil, &v, 200)
	return v
}

func (c *client) id(title string) string {
	c.t.Helper()
	for _, x := range c.view().Cells {
		if x.Title == title {
			return x.ID
		}
	}
	c.t.Fatalf("no cell titled %q", title)
	return ""
}

func (c *client) run(title string) (v manager.CellView) {
	c.t.Helper()
	c.do("POST", "/api/cells/"+c.id(title)+"/run", nil, &v, 200)
	return v
}

func text(v manager.CellView) string {
	var b strings.Builder
	for _, o := range v.State.Outputs {
		b.WriteString(o.Text)
	}
	return b.String()
}

type vars struct {
	Variables []struct {
		Name, Type, Value, Reason string
		Stale                     bool
	}
	Types []struct {
		Name    string
		Version int
	}
}

func TestStructEditOverHTTP(t *testing.T) {
	c := start(t, source)
	var all struct{ OK bool }
	c.do("POST", "/api/run-all", nil, &all, 200)
	if !all.OK {
		t.Fatalf("run-all failed: %+v", c.view().Cells)
	}
	show := c.run("Show")
	if text(show) != "Ada 1\n" || show.State.DurationMS <= 0 || show.State.Count == 0 {
		t.Errorf("%+v", show.State)
	}

	// Edit the struct and run its cell.
	var v manager.CellView
	c.do("POST", "/api/cells/"+c.id("User")+"/run", map[string]string{"source": "type User struct {\n\tName string\n\tAge int\n}"}, &v, 200)
	if v.State.Error != nil || !strings.Contains(v.Source, "Age  int") {
		t.Fatalf("%+v", v)
	}
	var vs vars
	c.do("GET", "/api/variables", nil, &vs, 200)
	if len(vs.Variables) != 2 || len(vs.Types) != 1 || vs.Types[0].Version != 2 {
		t.Fatalf("%+v", vs)
	}
	for _, x := range vs.Variables {
		if x.Stale != (x.Name == "u") {
			t.Errorf("%+v", x)
		}
	}
	if count := c.run("Count"); count.State.Error != nil {
		t.Errorf("unrelated cell: %+v", count.State.Error)
	}
	show = c.run("Show")
	create, _ := findTitle(c.view(), "Create")
	if show.State.Error == nil || show.State.Error.Kind != "stale" || show.State.Error.Stale[0].Cell != create.Num {
		t.Fatalf("%+v", show.State.Error)
	}
	if len(create.State.Stale) != 1 {
		t.Errorf("creator cell is not flagged: %+v", create.State)
	}
	var rs struct {
		OK    bool
		Cells []string
	}
	c.do("POST", "/api/run-stale", nil, &rs, 200)
	if !rs.OK || len(rs.Cells) != 1 {
		t.Errorf("%+v", rs)
	}
	if show = c.run("Show"); text(show) != "Ada 1\n" {
		t.Errorf("%+v", show.State)
	}
}

func findTitle(v manager.View, title string) (manager.CellView, bool) {
	for _, c := range v.Cells {
		if c.Title == title {
			return c, true
		}
	}
	return manager.CellView{}, false
}

func TestEditingWritesTheFile(t *testing.T) {
	c := start(t, source)
	var v manager.CellView
	c.do("POST", "/api/cells", map[string]string{"after": c.id("Show"), "kind": "code", "title": "New", "source": "fmt.Println(double(n))"}, &v, 201)
	c.do("POST", "/api/cells", map[string]string{"after": c.id("User"), "kind": "code", "title": "Fn", "source": "func double(i int) int { return 2*i }"}, &v, 201)
	c.do("PATCH", "/api/cells/"+v.ID, map[string]string{"title": "Double"}, &v, 200)
	c.do("POST", "/api/cells/"+c.id("New")+"/move", map[string]int{"delta": -1}, nil, 200)
	c.do("POST", "/api/cells/"+c.id("New")+"/move", map[string]int{"delta": 1}, nil, 200)
	b, _ := os.ReadFile(c.path)
	if !strings.Contains(string(b), "// %% Double\nfunc double(i int) int { return 2 * i }") {
		t.Errorf("file:\n%s", b)
	}
	var br manager.BuildResult
	c.do("POST", "/api/build", nil, &br, 200)
	if !br.OK {
		t.Fatalf("%+v", br)
	}
	bin := filepath.Join(t.TempDir(), "nb")
	c.do("POST", "/api/build", map[string]string{"output": bin}, &br, 200)
	if out, err := exec.Command(bin).Output(); err != nil || string(out) != "Ada 1\n2\n" {
		t.Errorf("%q %v", out, err)
	}
	// Interactively fine, but not for go build.
	c.do("POST", "/api/cells", map[string]string{"after": c.id("New"), "title": "Unused", "source": "unused := 1"}, &v, 201)
	c.do("POST", "/api/run-all", nil, nil, 200)
	if v := c.run("Unused"); v.State.Error != nil {
		t.Errorf("%+v", v.State.Error)
	}
	c.do("POST", "/api/build", nil, &br, 200)
	if br.OK || !strings.Contains(br.Output, "unused") {
		t.Errorf("%+v", br)
	}
	c.do("DELETE", "/api/cells/"+c.id("Unused"), nil, nil, 204)
	c.do("DELETE", "/api/cells/c999", nil, nil, 404)
	c.do("POST", "/api/cells/c999/run", nil, nil, 404)
	c.do("POST", "/api/cells", map[string]string{"kind": "nope"}, nil, 400)
}

func TestExternalEditsAreNoticed(t *testing.T) {
	c := start(t, source)
	id := c.id("Show")
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(c.path, []byte(strings.Replace(source, "n := 1", "n := 5", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if cell, _ := findTitle(c.view(), "Count"); cell.Source != "n := 5" {
		t.Errorf("%+v", cell)
	}
	if c.id("Show") != id {
		t.Error("cell id changed")
	}
}

func TestRestartAndInterrupt(t *testing.T) {
	c := start(t, source)
	c.do("POST", "/api/run-all", nil, nil, 200)
	var k manager.KernelView
	c.do("POST", "/api/kernel/restart", nil, &k, 200)
	var vs vars
	c.do("GET", "/api/variables", nil, &vs, 200)
	if k.Status != "ready" || len(vs.Variables) != 0 {
		t.Errorf("%+v %+v", k, vs)
	}
	// Restart ends a cell that never would.
	done := make(chan manager.CellView)
	id := c.id("Count")
	go func() {
		var v manager.CellView
		c.do("POST", "/api/cells/"+id+"/run", map[string]string{"source": "for {\n}"}, &v, 200)
		done <- v
	}()
	for i := 0; c.view().Kernel.Status != "busy" && i < 500; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	c.do("POST", "/api/kernel/restart", nil, &k, 200)
	if v := <-done; v.State.Status != "error" {
		t.Errorf("%+v", v.State)
	}
	c.run("Imports")
	if v := c.run("User"); v.State.Error != nil {
		t.Errorf("%+v", v.State.Error)
	}
}

func TestSyncDeps(t *testing.T) {
	c := start(t, source)
	c.do("POST", "/api/run-all", nil, nil, 200)
	var r struct{ Rebuilt bool }
	c.do("POST", "/api/kernel/sync-deps", nil, &r, 200)
	if r.Rebuilt {
		t.Error("rebuilt without need")
	}
	var v manager.CellView
	c.do("POST", "/api/cells", map[string]string{"after": c.id("Imports"), "title": "Bad", "source": `import "example.invalid/x"`}, &v, 201)
	if v := c.run("Bad"); v.State.Error == nil || v.State.Error.Kind != "needs_sync" {
		t.Errorf("%+v", v.State.Error)
	}
	if v := c.run("Count"); v.State.Error != nil {
		t.Errorf("state was lost: %+v", v.State.Error)
	}
}

// A package of the notebook's own module becomes available by a sync, and
// cells that had run are replayed.
func TestSyncNewLocalPackage(t *testing.T) {
	c := start(t, "")
	dir := filepath.Dir(c.path)
	write := func(name, text string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/proj\n\ngo 1.22\n")
	write("internal/greet/greet.go", "package greet\n\ntype G struct{ Name string }\n\nfunc (g G) Hello() string { return \"hello \" + g.Name }\n")
	var v manager.CellView
	ids := c.view().Cells
	c.do("POST", "/api/cells/"+ids[1].ID+"/run", nil, &v, 200)
	c.do("POST", "/api/cells/"+ids[2].ID+"/run", map[string]string{"source": "x := 41"}, &v, 200)
	var im, use manager.CellView
	c.do("POST", "/api/cells", map[string]string{"after": ids[1].ID, "title": "Imp", "source": `import "example.com/proj/internal/greet"`}, &im, 201)
	c.do("POST", "/api/cells", map[string]string{"after": ids[2].ID, "title": "Use", "source": "g := greet.G{Name: \"you\"}\nfmt.Println(g.Hello(), x+1)"}, &use, 201)
	if v := c.run("Imp"); v.State.Error == nil || v.State.Error.Kind != "needs_sync" {
		t.Fatalf("%+v", v.State.Error)
	}
	var r struct{ Rebuilt bool }
	c.do("POST", "/api/kernel/sync-deps", map[string]bool{"replay": true}, &r, 200)
	if !r.Rebuilt {
		t.Error("not rebuilt")
	}
	if v := c.run("Imp"); v.State.Error != nil {
		t.Fatalf("%+v", v.State.Error)
	}
	if v := c.run("Use"); text(v) != "hello you 42\n" {
		t.Errorf("%+v", v.State)
	}
}

func TestEventsStreamOutput(t *testing.T) {
	c := start(t, source)
	resp, err := http.Get(c.srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go c.do("POST", "/api/run-all", nil, nil, 200)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"type":"output"`) && strings.Contains(sc.Text(), "Ada 1") {
			return
		}
	}
	t.Error("no output event")
}

func TestOtherSitesAreRefused(t *testing.T) {
	c := start(t, source)
	for _, h := range []map[string]string{{"Origin": "https://evil.example"}, {"Host": "evil.example"}, {"Sec-Fetch-Site": "cross-site"}} {
		req, _ := http.NewRequest("POST", c.srv.URL+"/api/run-all", nil)
		for k, v := range h {
			req.Header.Set(k, v)
			if k == "Host" {
				req.Host = v
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%v: %s", h, resp.Status)
		}
	}
	resp, err := http.Get(c.srv.URL + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("index: %v %v", resp, err)
	}
}

const syncSource = `package main

// %% [markdown]
// # Products

// %% Imports
import "fmt"

// %% Struct
type Product struct {
	Name string
}

// %% Method
func (p Product) Show() string { return fmt.Sprint("<", p.Name, ">") }

func main() {
	// %% [markdown]
	// main

	// %% Counter
	counter := 1

	// %% Create
	products := []Product{{"pen"}}

	// %% Print
	fmt.Println(products[0].Show(), counter)
}
`

const sqliteImports = "import (\n\t\"database/sql\"\n\t\"fmt\"\n\n\t_ \"github.com/mattn/go-sqlite3\"\n)"

// startModule is start with a go.mod that requires what this repository does.
func startModule(t *testing.T, src string) *client {
	t.Helper()
	c := start(t, src)
	for _, f := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		b = bytes.Replace(b, []byte("module go-notebooks"), []byte("module example.com/shop"), 1)
		if err := os.WriteFile(filepath.Join(filepath.Dir(c.path), f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make the first kernel lack sqlite: nothing in the module imports it.
	mod := filepath.Join(filepath.Dir(c.path), "go.mod")
	b, _ := os.ReadFile(mod)
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "go-sqlite3") {
			l += " // indirect"
		}
		keep = append(keep, l)
	}
	if err := os.WriteFile(mod, []byte(strings.Join(keep, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *client) runAll(wantOK bool) {
	c.t.Helper()
	var all struct{ OK bool }
	c.do("POST", "/api/run-all", nil, &all, 200)
	if all.OK != wantOK {
		c.t.Fatalf("run-all ok = %v: %+v", all.OK, c.view().Cells)
	}
}

// Replay follows the document, not the order of execution: the edited import
// cell was run last but everything depends on it.
func TestSyncReplaysInDocumentOrder(t *testing.T) {
	c := startModule(t, syncSource)
	c.runAll(true)
	// A cell that failed before the sync must not stop the replay, either.
	var v manager.CellView
	c.do("POST", "/api/cells", map[string]string{"after": c.id("Counter"), "title": "Bad", "source": "panic(\"always\")"}, &v, 201)
	c.run("Bad")
	c.run("Counter")

	c.do("POST", "/api/cells/"+c.id("Imports")+"/run", map[string]string{"source": sqliteImports}, &v, 200)
	if v.State.Error == nil || v.State.Error.Kind != "needs_sync" || v.State.Error.Imports[0] != "github.com/mattn/go-sqlite3" {
		t.Fatalf("%+v", v.State.Error)
	}
	if v := c.run("Print"); text(v) != "<pen> 1\n" {
		t.Errorf("state was lost before the sync: %+v", v.State)
	}
	var r struct{ Rebuilt bool }
	c.do("POST", "/api/kernel/sync-deps", map[string]bool{"replay": true}, &r, 200)
	if !r.Rebuilt {
		t.Error("not rebuilt")
	}
	last := 0
	for _, x := range c.view().Cells {
		if x.Kind != "code" {
			continue
		}
		if x.State.Count <= last {
			t.Errorf("%s: execution %d after %d", x.Title, x.State.Count, last)
		}
		last = x.State.Count
		if want := map[bool]string{true: "error", false: "ok"}[x.Title == "Bad"]; x.State.Status != want {
			t.Errorf("%s: %s %+v", x.Title, x.State.Status, x.State.Error)
		}
	}
	if p, _ := findTitle(c.view(), "Print"); text(p) != "<pen> 1\n" {
		t.Errorf("%+v", p.State)
	}
	var vs vars
	c.do("GET", "/api/variables", nil, &vs, 200)
	got := map[string]string{}
	for _, x := range vs.Variables {
		got[x.Name] = x.Value
	}
	if len(got) != 2 || got["counter"] != "1" || got["products"] != `[]Product{Product{Name: "pen"}}` {
		t.Errorf("%v", got)
	}
	// The new dependency works.
	c.do("POST", "/api/cells", map[string]string{"after": c.id("Print"), "title": "DB", "source": "db, err := sql.Open(\"sqlite3\", \":memory:\")\nfmt.Println(db.Ping(), err)"}, &v, 201)
	if v := c.run("DB"); text(v) != "<nil> <nil>\n" {
		t.Errorf("%+v", v.State)
	}
}

// With nothing but declarations in memory the kernel is replaced at once; the
// declarations are repeated around the import cell.
func TestAutomaticSyncOfDeclarations(t *testing.T) {
	c := startModule(t, syncSource)
	for _, s := range []string{"Imports", "Struct", "Method"} {
		c.run(s)
	}
	var v manager.CellView
	c.do("POST", "/api/cells/"+c.id("Imports")+"/run", map[string]string{"source": sqliteImports}, &v, 200)
	if v.State.Error != nil {
		t.Fatalf("%+v", v.State.Error)
	}
	for _, s := range []string{"Counter", "Create", "Print"} {
		if v = c.run(s); v.State.Error != nil {
			t.Fatalf("%s: %+v", s, v.State.Error)
		}
	}
	if text(v) != "<pen> 1\n" {
		t.Errorf("%+v", v.State)
	}
	// An import that cannot be resolved leaves the running kernel alone.
	c2 := start(t, syncSource)
	for _, s := range []string{"Imports", "Struct", "Method"} {
		c2.run(s)
	}
	c2.do("POST", "/api/cells/"+c2.id("Imports")+"/run", map[string]string{"source": "import (\n\t\"fmt\"\n\n\t\"example.invalid/x\"\n)"}, &v, 200)
	if v.State.Error == nil || v.State.Error.Kind != "needs_sync" {
		t.Errorf("%+v", v.State.Error)
	}
	for _, s := range []string{"Create", "Counter", "Print"} {
		if v = c2.run(s); v.State.Error != nil {
			t.Errorf("%s: %+v", s, v.State.Error)
		}
	}
}
