package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-notebooks/internal/builder"
	"go-notebooks/kernel/proto"
)

var (
	kernelOnce sync.Once
	testKernel *builder.Kernel
	kernelErr  error
)

// nb drives a real kernel process, cell by cell.
type nb struct {
	t     *testing.T
	p     *Process
	cells map[string]int
}

func newNB(t *testing.T) *nb {
	t.Helper()
	kernelOnce.Do(func() {
		b, err := builder.New(filepath.Join("..", "..", "examples", "sqlite", "sqlite.go"), os.Stderr)
		if err != nil {
			kernelErr = err
			return
		}
		testKernel, kernelErr = b.Build(context.Background(), []string{"fmt", "github.com/mattn/go-sqlite3"})
	})
	if kernelErr != nil {
		t.Fatal(kernelErr)
	}
	p, err := Start(testKernel, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return &nb{t: t, p: p, cells: map[string]int{}}
}

type result struct {
	proto.Response
	out, errOut string
}

// run executes a cell; ids "c1", "c2" … have the numbers 1, 2 …
func (n *nb) run(id, src string) result {
	n.t.Helper()
	num := 0
	for _, r := range id[1:] {
		num = num*10 + int(r-'0')
	}
	n.cells[id] = num
	var r result
	var err error
	var mu sync.Mutex
	finished := false
	r.Response, err = n.p.Exec(id, src, n.cells, func(stream, text string) {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return // a goroutine or deferred call after the cell
		}
		if stream == "stdout" {
			r.out += text
		} else {
			r.errOut += text
		}
	})
	mu.Lock()
	finished = true
	mu.Unlock()
	if err != nil {
		n.t.Fatalf("cell %s: kernel: %v\n%s", id, err, r.errOut)
	}
	return r
}

func (n *nb) ok(id, src, want string) result {
	n.t.Helper()
	r := n.run(id, src)
	if r.Error != nil {
		n.t.Fatalf("cell %s: %s: %s (line %d)\n%s\n%s", id, r.Error.Kind, r.Error.Message, r.Error.Line, src, r.errOut)
	}
	if r.out != want {
		n.t.Errorf("cell %s: output %q, want %q\n%s", id, r.out, want, src)
	}
	return r
}

func (n *nb) fail(id, src, kind string, want ...string) result {
	n.t.Helper()
	r := n.run(id, src)
	if r.Error == nil {
		n.t.Fatalf("cell %s: no error (output %q), want %s\n%s", id, r.out, kind, src)
	}
	if r.Error.Kind != kind {
		n.t.Errorf("cell %s: error kind %s (%s), want %s", id, r.Error.Kind, r.Error.Message, kind)
	}
	for _, w := range want {
		if !strings.Contains(r.Error.Message, w) {
			n.t.Errorf("cell %s: error %q does not mention %q", id, r.Error.Message, w)
		}
	}
	if strings.Contains(r.Error.Message, "ᐟ") {
		n.t.Errorf("cell %s: internal name in %q", id, r.Error.Message)
	}
	return r
}

func (n *nb) vars() map[string]proto.Variable {
	n.t.Helper()
	resp, err := n.p.Variables(n.cells)
	if err != nil {
		n.t.Fatal(err)
	}
	out := map[string]proto.Variable{}
	for _, v := range resp.Variables {
		out[v.Name] = v
	}
	return out
}

func TestStateAndUnusedVariables(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import ("fmt"; "strings"; "os")`, "")
	n.ok("c2", "x := 42\nunused := \"nobody reads me\"", "")
	n.ok("c3", `fmt.Println(x + 1)`, "43\n")
	n.ok("c4", `nums := []int{1, 2}; m := map[string]int{"a": 1}; ch := make(chan int, 2); f := func(i int) int { return i * x }`, "")
	n.ok("c5", `nums = append(nums, 3); m["b"] = 2; ch <- 7`, "")
	n.ok("c6", `fmt.Println(nums, m, <-ch, f(2), strings.ToUpper("k"))`, "[1 2 3] map[a:1 b:2] 7 84 K\n")
	r := n.ok("c7", `fmt.Fprintln(os.Stderr, "oops")`, "")
	if r.errOut != "oops\n" {
		t.Errorf("stderr %q", r.errOut)
	}
	v := n.vars()
	if v["x"].Type != "int" || v["x"].Value != "42" || v["x"].CellID != "c2" {
		t.Errorf("x = %+v", v["x"])
	}
	if v["unused"].Value != `"nobody reads me"` {
		t.Errorf("unused = %+v", v["unused"])
	}
	if v["nums"].Value != "[]int{1, 2, 3}" || v["m"].Value != `map[string]int{"a": 1, "b": 2}` {
		t.Errorf("%+v %+v", v["nums"], v["m"])
	}
}

func TestRedeclaration(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `x := 10`, "")
	n.ok("c3", `func double() int { return x * 2 }`, "")
	n.ok("c2", `x := 11`, "")
	n.ok("c4", `fmt.Println(x, double())`, "11 22\n")
	// Same cell, new type: functions that use x are rebound or explained.
	n.ok("c2", `x := 1.5`, "")
	n.ok("c5", `fmt.Println(x)`, "1.5\n")
	n.fail("c4", `fmt.Println(double())`, proto.ErrStale, "double", "no longer compiles", "cell #3")
	n.ok("c2", `x := 12`, "")
	n.ok("c4", `fmt.Println(double())`, "24\n")
	n.ok("c6", `var y int = 5
var z string`, "")
	n.ok("c6", `var y int
var z = "w"`, "")
	n.ok("c7", `fmt.Println(y, z)`, "0 w\n")
	n.ok("c8", `a, err := fmt.Sscan("3", &y)
if err != nil { panic(err) }
b, err := 4, error(nil)
fmt.Println(a, b, y, err)`, "1 4 3 <nil>\n")
	n.ok("c8", `a, err := fmt.Sscan("5", &y); fmt.Println(a, err, y)`, "1 <nil> 5\n")
}

func TestFunctionsAndMethodsCanBeEdited(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `type User struct { Name string; age int }`, "")
	n.ok("c3", `func (u User) Hi(p string, xs ...int) string { return p + u.Name + fmt.Sprint(len(xs)) }
func (u *User) Birthday() { u.age++ }
func greet(u User) string { return u.Hi("hi ") }`, "")
	n.ok("c4", `u := User{Name: "Ada", age: 3}; u.Birthday(); fmt.Println(greet(u), u.age, u.Hi("", 1, 2))`, "hi Ada0 4 Ada2\n")
	n.ok("c3", `func (u User) Hi(p string, xs ...int) string { return p + "dear " + u.Name }
func (u *User) Birthday() { u.age += 10 }
func greet(u User) string { return u.Hi("hi ") }`, "")
	// The method bodies changed; u is still the same live value.
	n.ok("c5", `u.Birthday(); fmt.Println(greet(u), u.age)`, "hi dear Ada 14\n")
	if n.vars()["u"].Stale {
		t.Error("u is stale after a method body changed")
	}
	// New signature for a function.
	n.ok("c6", `func add(a, b int) int { return a + b }
func twice(a int) int { return add(a, a) }`, "")
	n.ok("c7", `func add(a, b int) int { return a + b + 100 }`, "")
	n.ok("c8", `fmt.Println(twice(1))`, "102\n")
	n.ok("c7", `func add(a int) int { return a }`, "")
	n.fail("c8", `fmt.Println(twice(1))`, proto.ErrStale, "twice", "cell #6")
	n.ok("c9", `fmt.Println(add(1), u.Name)`, "1 Ada\n")
	// Recursion, constants, named types, interfaces.
	n.ok("c10", `const ( A = iota + 1; B; C )
type Color int
func (c Color) String() string { return [...]string{"", "red", "green", "blue"}[c] }
type Namer interface { Title() string }
func (u User) Title() string { return "Dr " + u.Name }
func fib(n int) int { if n < 2 { return n }; return fib(n-1) + fib(n-2) }`, "")
	n.ok("c11", `var nm Namer = u
fmt.Println(Color(B), nm.Title(), fib(15))`, "green Dr Ada 610\n")
	n.ok("c10", `const ( A = iota + 2; B; C )`, "")
	n.ok("c12", `fmt.Println(A, B)`, "2 3\n")
}

func TestStructEvolution(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `type User struct {
	Name string
	Score int
}`, "")
	n.ok("c3", `func describe(u User) string { return u.Name + "!" }`, "")
	n.ok("c4", `u := User{Name: "Ada", Score: 1}
users := []*User{&u}
byName := map[string]User{"ada": u}`, "")
	n.ok("c5", `counter := 1`, "")
	n.ok("c6", `fmt.Println(describe(u), len(users))`, "Ada! 1\n")

	// Add a field.
	r := n.ok("c2", `type User struct {
	Name string
	Score int
	Age int
}`, "")
	if len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "User v1 -> v2: added Age int") {
		t.Errorf("notices %q", r.Notices)
	}
	n.ok("c5", `counter++; fmt.Println(counter)`, "2\n") // unrelated state lives on
	n.ok("c5", `counter++; fmt.Println(counter)`, "3\n")
	for _, src := range []string{`fmt.Println(u.Age)`, `fmt.Println(describe(u))`, `fmt.Println(u)`} {
		r := n.fail("c6", src, proto.ErrStale, "underlying struct `User` changed (v1 -> v2: added Age int)", "variable `u` is stale", "re-run cell #4 to update `u`")
		if len(r.Error.Stale) != 1 || r.Error.Stale[0].CellID != "c4" {
			t.Errorf("%+v", r.Error.Stale)
		}
	}
	n.fail("c6", `fmt.Println(len(users), byName)`, proto.ErrStale, "`users`", "`byName`")
	v := n.vars()
	for _, name := range []string{"u", "users", "byName"} {
		if !v[name].Stale || v[name].Versions[0] != "User v1" || len(v[name].Changes) != 1 {
			t.Errorf("%s = %+v", name, v[name])
		}
	}
	if v["counter"].Stale {
		t.Error("counter is stale")
	}
	// Re-running the creator recovers.
	n.ok("c4", `u := User{Name: "Ada", Score: 1, Age: 36}
users := []*User{&u}
byName := map[string]User{"ada": u}`, "")
	n.ok("c6", `fmt.Println(describe(u), u.Age, users[0].Age, byName["ada"].Age, counter)`, "Ada! 36 36 36 3\n")
	if v := n.vars()["u"]; v.Stale || v.Versions[0] != "User v2" || v.Value != `User{Name: "Ada", Score: 1, Age: 36}` {
		t.Errorf("u = %+v", v)
	}

	// Change a field's type and remove another.
	n.ok("c2", `type User struct {
	Name string
	Age string
}`, "")
	n.fail("c6", `fmt.Println(u.Age)`, proto.ErrStale, "v2 -> v3", "changed Age: int -> string", "removed Score int")
	n.fail("c4", `u := User{Name: "Ada", Score: 1, Age: 36}`, proto.ErrCompile, "Score")
	n.ok("c5", `counter++; fmt.Println(counter)`, "4\n")
	n.ok("c4", `u := User{Name: "Ada", Age: "old"}`, "")
	n.ok("c6", `fmt.Println(describe(u), u.Age)`, "Ada! old\n")
	// Re-running an unchanged struct cell changes nothing.
	n.ok("c2", `type User struct {
	Name string // a comment
	Age  string
}`, "")
	n.ok("c6", `fmt.Println(u.Age)`, "old\n")
}

func TestStructEvolutionReachesDependents(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `type User struct { Name string }
func (u User) Hello() string { return "hello " + u.Name }`, "")
	n.ok("c3", `type Team struct { Members []User; Lead *User }
type Admin struct { User; Level int }`, "")
	n.ok("c4", `team := Team{Members: []User{{"Ada"}}}
admin := Admin{User: User{"Root"}, Level: 1}`, "")
	n.ok("c5", `func total() int { return len(team.Members) }
func mk(name string) User { return User{Name: name} }`, "")
	n.ok("c6", `var any1 any = mk("dyn"); lazy := func() User { return User{Name: "x"} }; n := 1; ifaces := []fmt.Stringer{}`, "")
	n.ok("c7", `fmt.Println(total(), admin.Hello(), admin.User.Name, mk("z").Hello())`, "1 hello Root Root hello z\n")
	n.ok("c2", `type User struct { Name string; Admin bool }
func (u User) Hello() string { return "hello " + u.Name }`, "")
	n.fail("c8", `fmt.Println(team)`, proto.ErrStale, "`User` changed (v1 -> v2", "`team`", "cell #4")
	n.fail("c8", `fmt.Println(admin.Level)`, proto.ErrStale, "`admin`")
	n.fail("c8", `fmt.Println(total())`, proto.ErrStale, "`total` uses `team`", "cell #4")
	n.fail("c8", `fmt.Println(any1)`, proto.ErrStale, "`any1`")
	n.fail("c8", `fmt.Println(lazy())`, proto.ErrStale, "`lazy`")
	n.ok("c8", `fmt.Println(n, mk("y").Hello(), mk("y").Admin)`, "1 hello y false\n")
	// Re-creating team and using the function that depends on it in one cell.
	n.ok("c4", `team := Team{Members: []User{{"Ada", true}, {"Bob", false}}}
admin := Admin{User: User{"Root", true}, Level: 2}
fmt.Println(total(), admin.Hello(), admin.User.Admin, admin.Level)`, "2 hello Root true 2\n")
	n.ok("c8", `fmt.Println(total())`, "2\n")
	n.ok("c8", `any1 = mk("again"); fmt.Println(any1.(User).Name)`, "again\n")
}

func TestAssignmentRefreshesStaleVariable(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `type P struct { X int }`, "")
	n.ok("c3", `var p P`, "")
	n.ok("c4", `p = P{X: 1}`, "")
	n.ok("c2", `type P struct { X, Y int }`, "")
	n.fail("c5", `p.X++`, proto.ErrStale, "`p`")
	n.ok("c4", `p = P{X: 1, Y: 2}`, "")
	n.ok("c5", `fmt.Println(p.Y)`, "2\n")
}

func TestBrokenDependentsRecover(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `type P struct { X int }`, "")
	n.ok("c3", `func (p P) Double() int { return p.X * 2 }
func sum(a, b P) int { return a.X + b.X }`, "")
	n.ok("c4", `fmt.Println(P{2}.Double(), sum(P{1}, P{2}))`, "4 3\n")
	n.ok("c2", `type P struct { Y int }`, "") // removes X: c3 cannot compile
	n.fail("c4", `fmt.Println(sum(P{1}, P{2}))`, proto.ErrStale, "`sum` no longer compiles", "cell #3")
	n.ok("c5", `fmt.Println(P{Y: 4}.Y)`, "4\n")
	n.ok("c2", `type P struct { X, Y int }`, "")
	n.ok("c4", `fmt.Println(P{X: 2}.Double(), sum(P{X: 1}, P{X: 2}))`, "4 3\n")
}

func TestErrorsDoNotKillTheKernel(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import ("fmt"; "time"; "errors")`, "")
	n.ok("c2", `keep := 7`, "")
	if r := n.fail("c3", "a := 1\nb := a +\n", proto.ErrSyntax); r.Error.Line == 0 {
		t.Error("no line")
	}
	if r := n.fail("c3", "a := 1\n\nfmt.Println(undefinedThing)", proto.ErrCompile, "undefined: undefinedThing"); r.Error.Line != 3 {
		t.Errorf("line %d", r.Error.Line)
	}
	n.fail("c3", `os.Exit(1)`, proto.ErrCompile, `missing import "os"?`)
	if r := n.fail("c3", "done := true\nvar s []int\nfmt.Println(s[3])\nnever := 1", proto.ErrPanic, "out of range"); r.Error.Line != 3 {
		t.Errorf("line %d", r.Error.Line)
	}
	if v := n.vars(); v["done"].Value != "true" || v["never"].Name != "" {
		t.Errorf("variables after a panic: %v", v)
	}
	n.ok("c4", "type T struct { p *int; N int }\nfunc (t *T) get() int { return t.N }", "")
	n.fail("c5", "var t *T\nfmt.Println(t.get())", proto.ErrPanic, "nil pointer")
	n.fail("c5", `panic(errors.New("boom"))`, proto.ErrPanic, "boom")
	n.fail("c5", `panic("text")`, proto.ErrPanic, "text")
	r := n.ok("c5", `go func() { panic("in goroutine") }()
time.Sleep(50 * time.Millisecond)`, "")
	if !strings.Contains(r.errOut, "in goroutine") {
		t.Errorf("stderr %q", r.errOut)
	}
	n.ok("c6", `import "os"`, "")
	n.fail("c6", `os.Exit(3)`, proto.ErrPanic)
	n.fail("c6", `import "example.com/nope"`, proto.ErrNeedSync, "example.com/nope")
	n.ok("c7", `fmt.Println(keep)`, "7\n")
}

func TestDisplayAndHelpers(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import ("fmt"; "errors"; "encoding/json"; "strings")`, "")
	n.ok("c2", `type P struct { X int; name string }
type E struct { Code int }
func (e *E) Error() string { return fmt.Sprint("code ", e.Code) }
func find(i int) error { if i == 0 { return nil }; return &E{i} }`, "")
	show := func(src, want string) {
		t.Helper()
		if r := n.ok("c3", src, ""); r.Result != want {
			t.Errorf("%s shows %q, want %q", src, r.Result, want)
		}
	}
	show(`p := P{1, "n"}
p`, `P{X: 1, name: "n"}`)
	show(`1 + 2`, `3`)
	show(`strings.Fields(" a b ")`, `[]string{"a", "b"}`)
	show(`find(0)`, ``)
	show(`find(2)`, `code 2`)
	show(`len("abc")`, `3`)
	show(`[]P{{X: 2}}`, `[]P{P{X: 2, name: ""}}`)
	show(`map[string]any{"a": p, "b": 1}`, `map[string]any{"a": P{X: 1, name: "n"}, "b": 1}`)
	n.ok("c4", `fmt.Println(min(3, 1, 2), max(1.5, 2)); m := map[int]int{1: 1}; clear(m); fmt.Println(len(m))`, "1 2\n0\n")
	n.ok("c5", `var v any = p
fmt.Println(v)
b, err := json.Marshal(map[string]any{"k": []any{1, "x"}})
fmt.Println(string(b), err)`, "{1 n}\n{\"k\":[1,\"x\"]} <nil>\n")
	n.ok("c6", `w := fmt.Errorf("wrap: %w", &E{4})
fmt.Println(w, errors.Unwrap(w) != nil)`, "wrap: code 4 true\n")
	n.ok("c7", `func Map[T, U any](xs []T, f func(T) U) []U { var r []U; for _, x := range xs { r = append(r, f(x)) }; return r }`, "")
	n.ok("c8", `fmt.Println(Map([]int{1, 2}, func(i int) string { return fmt.Sprint(i * 2) }))`, "[2 4]\n")
	n.ok("c9", `for i := range 3 { defer func() {}(); _ = i }`, "")
}

func TestDefersWaitForRerun(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", "v := 1\ndefer fmt.Println(\"closing\", v)", "")
	n.ok("c3", `v = 5; fmt.Println("still open")`, "still open\n")
	n.ok("c2", "v := 2\ndefer fmt.Println(\"closing\", v)", "closing 1\n")
}

func TestCgoSQLiteStaysOpenAcrossCells(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import (
	"database/sql"
	"fmt"

	sqlite "github.com/mattn/go-sqlite3"
)`, "")
	n.ok("c2", `ver, _, _ := sqlite.Version()
fmt.Println(ver != "")`, "true\n")
	n.ok("c3", `db, err := sql.Open("sqlite3", ":memory:")
if err != nil { panic(err) }
db.SetMaxOpenConns(1)
defer db.Close()`, "")
	n.ok("c4", `type Row struct { ID int; Name string }
func load(db *sql.DB) ([]Row, error) {
	rows, err := db.Query("SELECT id, name FROM t ORDER BY id")
	if err != nil { return nil, err }
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Name); err != nil { return nil, err }
		out = append(out, r)
	}
	return out, rows.Err()
}`, "")
	n.ok("c5", `_, err = db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
fmt.Println(err)
for _, s := range []string{"ada", "bob"} {
	if _, err := db.Exec("INSERT INTO t (name) VALUES (?)", s); err != nil { panic(err) }
}`, "<nil>\n")
	n.ok("c6", `rows, err := load(db)
fmt.Println(rows, err)`, "[{1 ada} {2 bob}] <nil>\n")
	// The connection survives a struct edit.
	n.ok("c4", `type Row struct { ID int; Name string; Extra bool }`, "")
	n.fail("c7", `fmt.Println(rows)`, proto.ErrStale, "`rows`", "cell #6")
	n.ok("c6", `rows, err := load(db)
fmt.Println(len(rows), rows[1].Name, rows[1].Extra, err)`, "2 bob false <nil>\n")
	var count int
	_ = count
	n.ok("c8", `var count int
err = db.QueryRow("SELECT COUNT(*) FROM t").Scan(&count)
fmt.Println(count, err)`, "2 <nil>\n")
	if v := n.vars()["db"]; v.Type != "*sql.DB" {
		t.Errorf("db = %+v", v)
	}
}

func TestNewerStandardLibrary(t *testing.T) {
	n := newNB(t)
	// Packages and functions missing from Yaegi's own symbol tables.
	n.ok("c1", `import ("fmt"; "os/exec"; "strings"; "crypto/sha3"; "time")`, "")
	n.ok("c2", `sum := sha3.Sum256(nil)
out, err := exec.Command("echo", "hi").Output()
fmt.Printf("%s%v %x %v\n", out, err, sum[:2], strings.ContainsFunc("a", func(r rune) bool { return r == 'a' }))
d := 2 * time.Second
fmt.Println(d)`, "hi\n<nil> a7ff true\n2s\n")
}

func TestKernelDeathIsReported(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "os/exec"`, "")
	_, err := n.p.Exec("c2", `exec.Command("sh", "-c", "kill -9 $PPID").Run(); select {}`, n.cells, nil)
	if err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("err = %v", err)
	}
	if n.p.Alive() {
		t.Error("still alive")
	}
}

func TestConcurrency(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import ("fmt"; "sync")`, "")
	n.ok("c2", `jobs := make(chan int); results := make(chan int, 10)
var wg sync.WaitGroup
func worker(id int, jobs <-chan int, results chan<- int, wg *sync.WaitGroup) {
	defer wg.Done()
	for j := range jobs { results <- j * j }
}
for i := 0; i < 3; i++ { wg.Add(1); go worker(i, jobs, results, &wg) }`, "")
	n.ok("c3", `for i := 1; i <= 4; i++ { jobs <- i }
close(jobs); wg.Wait(); close(results)
sum := 0
for r := range results { sum += r }
fmt.Println(sum)`, "30\n")
}

func TestBackgroundOutputIsKept(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import ("fmt"; "time")`, "")
	got := make(chan string, 10)
	if _, err := n.p.Exec("c2", `go func() { time.Sleep(100 * time.Millisecond); fmt.Println("late") }()`, n.cells,
		func(_, text string) { got <- text }); err != nil {
		t.Fatal(err)
	}
	n.vars()
	select {
	case s := <-got:
		if s != "late\n" {
			t.Errorf("%q", s)
		}
	case <-time.After(5 * time.Second):
		t.Error("output lost")
	}
}

func TestIncompatibleStructEditIsAnnounced(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `import "fmt"`, "")
	n.ok("c2", `type Item struct { Name string; Price float64 }`, "")
	n.ok("c3", `func (it Item) Show() string { return fmt.Sprintf("%s: %.2f", it.Name, it.Price) }
func (it Item) Upper() string { return it.Name + "!" }`, "")
	n.ok("c4", `func print(it Item) { fmt.Println(it.Show()) }`, "")
	n.ok("c5", `it := Item{"pen", 1.5}
func count() int { return len(it.Name) }
print(it)`, "pen: 1.50\n")

	notice := func(r result, want ...string) {
		t.Helper()
		all := strings.Join(r.Notices, "\n")
		for _, w := range want {
			if !strings.Contains(all, w) {
				t.Errorf("notices do not say %q:\n%s", w, all)
			}
		}
		if strings.Contains(all, "ᐟ") || strings.Contains(all, "Item.Upper") {
			t.Errorf("notices:\n%s", all)
		}
	}
	src := `type Item struct { Name string; Cents int }`
	notice(n.ok("c2", src, ""),
		"Item v1 -> v2: added Cents int, removed Price float64",
		"Item.Show (cell #3) could not update for Item v2: it.Price undefined (type Item has no field or method Price) — edit and re-run cell #3",
		"print (cell #4) could not update for Item v2: it uses Item.Show (cell #3), which did not update either",
		"count (cell #5) could not update for Item v2: it uses stale variable it — re-run cell #5")
	// Using the method says why it is missing; the rest of the type works.
	n.ok("c6", `x := Item{"pen", 150}
fmt.Println(x.Upper())`, "pen!\n")
	r := n.fail("c7", `fmt.Println(x.Show())`, proto.ErrStale, "`Item.Show` no longer compiles", "Price", "cell #3")
	if r.Error.Stale[0].CellID != "c3" {
		t.Errorf("%+v", r.Error.Stale)
	}
	n.fail("c7", `var s fmt.Stringer; var sh interface{ Show() string } = x`, proto.ErrStale, "`Item.Show`", "cell #3")
	// An unrelated cell, or the same struct again, does not repeat it.
	if r := n.ok("c8", `y := 1`, ""); len(r.Notices) != 0 {
		t.Errorf("%q", r.Notices)
	}
	if r := n.ok("c2", src, ""); len(r.Notices) != 0 {
		t.Errorf("%q", r.Notices)
	}
	// Fixing the method, even with another signature, leaves x alone.
	n.ok("c3", `func (it Item) Show(unit string) string { return fmt.Sprintf("%s: %d%s", it.Name, it.Cents, unit) }
func (it Item) Upper() string { return it.Name + "!" }`, "")
	n.ok("c7", `fmt.Println(x.Show("c"))`, "pen: 150c\n")
	n.fail("c7", `print(x)`, proto.ErrStale, "`print` no longer compiles", "cell #4")
	n.ok("c4", `func print(it Item) { fmt.Println(it.Show("ct")) }`, "")
	n.ok("c7", `print(x)`, "pen: 150ct\n")
}

func TestVariableTypeChangeIsAnnounced(t *testing.T) {
	n := newNB(t)
	n.ok("c1", `x := 1`, "")
	n.ok("c2", `func double() int { return x * 2 }`, "")
	r := n.ok("c1", `x := "s"`, "")
	if len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "double (cell #2) could not update: ") {
		t.Errorf("%q", r.Notices)
	}
}
