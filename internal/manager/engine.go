package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-notebooks/internal/builder"
	"go-notebooks/internal/notebook"
	"go-notebooks/kernel/cellsrc"
	"go-notebooks/kernel/proto"
)

// Output is a stretch of text printed by a cell.
type Output struct {
	Stream string `json:"stream"` // stdout or stderr
	Text   string `json:"text"`
}

// State is the result of the last run of a cell.
type State struct {
	Status     string           `json:"status"` // "", queued, running, ok, error
	Outputs    []Output         `json:"outputs"`
	Result     string           `json:"result,omitempty"`
	Notices    []string         `json:"notices,omitempty"`
	Error      *proto.CellError `json:"error,omitempty"`
	DurationMS float64          `json:"duration_ms"`
	Count      int              `json:"count"`           // execution number in this session, 0 if not run
	Edited     bool             `json:"edited"`          // changed since it was run
	Stale      []string         `json:"stale,omitempty"` // stale variables this cell created
	ran        string
}

// CellView is a cell with its state, as served to clients.
type CellView struct {
	ID      string `json:"id"`
	Num     int    `json:"num"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Source  string `json:"source"`
	Section string `json:"section"`
	Warning string `json:"warning,omitempty"`
	State   State  `json:"state"`
}

// KernelView describes the kernel.
type KernelView struct {
	Status   string   `json:"status"` // none, building, ready, busy, dead, error
	Message  string   `json:"message,omitempty"`
	Packages []string `json:"packages"`
	Go       string   `json:"go,omitempty"`
}

// View is the whole notebook.
type View struct {
	File    string     `json:"file"`
	Path    string     `json:"path"`
	Package string     `json:"package"`
	Cells   []CellView `json:"cells"`
	Kernel  KernelView `json:"kernel"`
	Error   string     `json:"error,omitempty"`
}

// Event tells clients that something changed.
type Event struct {
	Type   string      `json:"type"` // cell, output, notebook, variables, kernel, log
	Cell   string      `json:"cell,omitempty"`
	Output *Output     `json:"output,omitempty"`
	View   *CellView   `json:"view,omitempty"`
	Kernel *KernelView `json:"kernel,omitempty"`
	Text   string      `json:"text,omitempty"`
}

const maxOutput = 1 << 20 // bytes of output kept per cell

// Engine is an open notebook with its kernel.
type Engine struct {
	Path string
	// OnOutput, if set, receives cell output as it appears.
	OnOutput func(Output)

	b *builder.Builder

	run sync.Mutex // held while the kernel is built, used or replaced

	mu      sync.Mutex // guards the rest
	nb      *notebook.Notebook
	stamp   time.Time
	size    int64
	loadErr string
	states  map[string]*State
	proc    *Process
	kernel  *builder.Kernel
	kview   KernelView
	count   int
	vars    []proto.Variable
	types   []proto.Type
	subs    map[chan Event]bool
	closed  bool
}

// Open loads a notebook, creating the file if it does not exist.
func Open(path string, log io.Writer) (*Engine, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(abs) != ".go" {
		return nil, fmt.Errorf("%s: notebooks are .go files", path)
	}
	e := &Engine{Path: abs, states: map[string]*State{}, subs: map[chan Event]bool{}, kview: KernelView{Status: "none", Packages: []string{}}}
	if log == nil {
		log = io.Discard
	}
	if e.b, err = builder.New(abs, io.MultiWriter(log, logEvents{e})); err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		nb := notebook.New()
		nb.Insert("", notebook.Markdown, "", "# "+strings.TrimSuffix(filepath.Base(abs), ".go"))
		c, _ := nb.Insert(nb.Cells[0].ID, notebook.Code, "", "import \"fmt\"")
		nb.Insert(c.ID, notebook.Code, "", "fmt.Println(\"hello\")")
		if err := nb.Save(abs); err != nil {
			return nil, err
		}
	}
	if e.nb, err = notebook.Load(abs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	e.remember()
	return e, nil
}

type logEvents struct{ e *Engine }

func (l logEvents) Write(p []byte) (int, error) {
	l.e.emit(Event{Type: "log", Text: string(p)})
	return len(p), nil
}

// Close stops the kernel.
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	p := e.proc
	e.proc = nil
	e.mu.Unlock()
	if p != nil {
		p.Stop()
	}
}

// Subscribe returns a channel of events and a function to end it. The channel
// is closed if its reader falls behind.
func (e *Engine) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 1024)
	e.mu.Lock()
	e.subs[ch] = true
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

func (e *Engine) emit(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitLocked(ev)
}

func (e *Engine) emitLocked(ev Event) {
	for ch := range e.subs {
		select {
		case ch <- ev:
		default: // a client that fell behind: end its stream, so it starts over
			delete(e.subs, ch)
			close(ch)
		}
	}
}

// remember notes the file's state after we read or wrote it.
func (e *Engine) remember() {
	if st, err := os.Stat(e.Path); err == nil {
		e.stamp, e.size = st.ModTime(), st.Size()
	}
}

// reload picks up changes made to the file by other programs.
func (e *Engine) reload() {
	st, err := os.Stat(e.Path)
	if err != nil || st.ModTime().Equal(e.stamp) && st.Size() == e.size {
		return
	}
	e.stamp, e.size = st.ModTime(), st.Size()
	p, err := notebook.Load(e.Path)
	if err != nil {
		e.loadErr = fmt.Sprintf("the file changed on disk but cannot be read as a notebook: %v", err)
		return
	}
	e.loadErr = ""
	e.nb.Adopt(p)
}

// Poll reloads the notebook if its file changed and reports whether it did.
func (e *Engine) Poll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	before := e.stamp
	e.reload()
	if !e.stamp.Equal(before) {
		e.emitLocked(Event{Type: "notebook"})
	}
}

// edit applies a change to the notebook and saves it.
func (e *Engine) edit(f func(nb *notebook.Notebook) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reload()
	if err := f(e.nb); err != nil {
		return err
	}
	e.nb.Refresh()
	if err := e.nb.Save(e.Path); err != nil {
		return err
	}
	e.remember()
	e.loadErr = ""
	e.emitLocked(Event{Type: "notebook"})
	return nil
}

// NotFoundError names an unknown cell.
type NotFoundError struct{ ID string }

func (e NotFoundError) Error() string { return fmt.Sprintf("no cell %q", e.ID) }

func (e *Engine) exists(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, _ := e.nb.Find(id); c == nil {
		return NotFoundError{id}
	}
	return nil
}

// Insert adds a cell.
func (e *Engine) Insert(after, kind, title, source string) (CellView, error) {
	if after != "" {
		if err := e.exists(after); err != nil {
			return CellView{}, err
		}
	}
	var id string
	err := e.edit(func(nb *notebook.Notebook) error {
		c, err := nb.Insert(after, kind, title, source)
		if err == nil {
			id = c.ID
		}
		return err
	})
	if err != nil {
		return CellView{}, err
	}
	return e.Cell(id)
}

// Update changes a cell.
func (e *Engine) Update(id string, kind, title, source *string) (CellView, error) {
	if err := e.exists(id); err != nil {
		return CellView{}, err
	}
	if err := e.edit(func(nb *notebook.Notebook) error { return nb.Update(id, kind, title, source) }); err != nil {
		return CellView{}, err
	}
	return e.Cell(id)
}

// Delete removes a cell. What it defined stays in memory.
func (e *Engine) Delete(id string) error {
	if err := e.exists(id); err != nil {
		return err
	}
	return e.edit(func(nb *notebook.Notebook) error { return nb.Delete(id) })
}

// Move reorders a cell.
func (e *Engine) Move(id string, delta int) error {
	if err := e.exists(id); err != nil {
		return err
	}
	return e.edit(func(nb *notebook.Notebook) error { return nb.Move(id, delta) })
}

// View returns the notebook.
func (e *Engine) View() View {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reload()
	v := View{File: filepath.Base(e.Path), Path: e.Path, Package: e.nb.Package(), Cells: []CellView{}, Kernel: e.kview, Error: e.loadErr}
	for i, c := range e.nb.Cells {
		v.Cells = append(v.Cells, e.view(i, c))
	}
	return v
}

// Cell returns one cell.
func (e *Engine) Cell(id string) (CellView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, i := e.nb.Find(id)
	if c == nil {
		return CellView{}, NotFoundError{id}
	}
	return e.view(i, c), nil
}

func (e *Engine) view(i int, c *notebook.Cell) CellView {
	v := CellView{ID: c.ID, Num: i + 1, Kind: c.Kind, Title: c.Title, Source: c.Source, Section: c.Section.String(), Warning: notebook.Warning(c)}
	if s := e.states[c.ID]; s != nil {
		v.State = *s
		v.State.Outputs = append([]Output{}, s.Outputs...)
		v.State.Edited = s.Count > 0 && strings.TrimSpace(s.ran) != strings.TrimSpace(c.Source)
	}
	if v.State.Outputs == nil {
		v.State.Outputs = []Output{}
	}
	for _, x := range e.vars {
		if x.Stale && x.CellID == c.ID {
			v.State.Stale = append(v.State.Stale, x.Name)
		}
	}
	return v
}

func (e *Engine) numbering() map[string]int {
	m := map[string]int{}
	for i, c := range e.nb.Cells {
		m[c.ID] = i + 1
	}
	return m
}

func (e *Engine) setKernel(k KernelView) {
	if k.Packages == nil {
		k.Packages = []string{}
	}
	e.mu.Lock()
	e.kview = k
	e.emitLocked(Event{Type: "kernel", Kernel: &k})
	e.mu.Unlock()
}

func (e *Engine) imports() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nb.Imports()
}

// ensure starts the kernel if none runs. The caller holds e.run.
func (e *Engine) ensure(ctx context.Context) error {
	e.mu.Lock()
	ok := e.proc != nil && e.proc.Alive()
	e.mu.Unlock()
	if ok {
		return nil
	}
	e.setKernel(KernelView{Status: "building", Message: "preparing kernel"})
	fail := func(err error) error {
		e.setKernel(KernelView{Status: "error", Message: err.Error()})
		return err
	}
	k, err := e.b.Build(ctx, e.imports())
	if err != nil {
		return fail(err)
	}
	p, err := Start(k, e.b.Dir())
	if err != nil {
		return fail(err)
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		p.Stop()
		return errors.New("closed")
	}
	e.proc, e.kernel = p, k
	e.count = 0
	e.vars, e.types = nil, nil
	for _, s := range e.states {
		s.Count = 0
	}
	e.mu.Unlock()
	e.setKernel(KernelView{Status: "ready", Packages: k.Info.Packages, Go: k.Info.GoVersion})
	return nil
}

// KernelError turns a failure to provide a kernel into a cell error.
func kernelError(err error) *proto.CellError {
	var m *builder.MissingError
	if errors.As(err, &m) {
		return &proto.CellError{Kind: proto.ErrNeedSync, Message: m.Error(), Imports: m.Imports}
	}
	return &proto.CellError{Kind: proto.ErrKernel, Message: err.Error()}
}

// RunCell executes a code cell; a non-nil source is saved first. Failures of
// the cell are part of the returned state.
func (e *Engine) RunCell(ctx context.Context, id string, source *string) (CellView, error) {
	if source != nil {
		if _, err := e.Update(id, nil, nil, source); err != nil {
			return CellView{}, err
		}
	}
	v, err := e.Cell(id)
	if err != nil || v.Kind != notebook.Code {
		return v, err
	}
	e.setStatus(id, "queued")
	e.run.Lock()
	defer e.run.Unlock()
	e.runCell(ctx, id)
	return e.Cell(id)
}

func (e *Engine) setStatus(id, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.states[id]
	if s == nil {
		s = &State{}
		e.states[id] = s
	}
	s.Status = status
	e.emitCell(id)
}

func (e *Engine) emitCell(id string) {
	if c, i := e.nb.Find(id); c != nil {
		v := e.view(i, c)
		e.emitLocked(Event{Type: "cell", Cell: id, View: &v})
	}
}

// runCell runs a cell; the caller holds e.run. It reports success.
func (e *Engine) runCell(ctx context.Context, id string) bool {
	e.mu.Lock()
	c, _ := e.nb.Find(id)
	if c == nil {
		e.mu.Unlock()
		return false
	}
	src := c.Source
	st := &State{Status: "running", Outputs: []Output{}, ran: src}
	if old := e.states[id]; old != nil {
		st.Count = old.Count
	}
	e.states[id] = st
	e.emitCell(id)
	e.mu.Unlock()

	finish := func(cerr *proto.CellError) bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		st.Error = cerr
		st.Status = "ok"
		if cerr != nil {
			st.Status = "error"
		}
		e.emitCell(id)
		return cerr == nil
	}
	resp, err := e.exec(ctx, id, src, st)
	if resp.Error != nil && resp.Error.Kind == proto.ErrNeedSync {
		// If no state would be lost and the package is available, switch to a
		// kernel that has it and repeat the declarations around this cell.
		if before, after, ok := e.onlyDeclarations(id); ok {
			if _, berr := e.b.Build(ctx, e.imports()); berr != nil {
				e.mu.Lock()
				st.Outputs = []Output{}
				e.mu.Unlock()
				err = berr
			} else {
				e.stopKernel()
				e.runList(ctx, before, false)
				if resp, err = e.exec(ctx, id, src, st); err == nil && resp.Error == nil {
					defer e.runList(ctx, after, false)
				}
			}
		}
	}
	if err != nil {
		e.mu.Lock()
		alive := e.proc != nil && e.proc.Alive()
		e.mu.Unlock()
		if !alive && errors.Is(err, ErrDied) {
			e.setKernel(KernelView{Status: "dead", Message: "the kernel process ended; it restarts with the next run, without its previous state"})
		}
		return finish(kernelError(err))
	}
	e.mu.Lock()
	st.Result, st.Notices = resp.Result, resp.Notices
	e.count++
	st.Count = e.count
	e.mu.Unlock()
	e.refreshVars()
	return finish(resp.Error)
}

func (e *Engine) exec(ctx context.Context, id, src string, st *State) (proto.Response, error) {
	if err := e.ensure(ctx); err != nil {
		return proto.Response{}, err
	}
	e.mu.Lock()
	p, nums, pkgs := e.proc, e.numbering(), e.kernel.Info
	e.mu.Unlock()
	if p == nil {
		return proto.Response{}, ErrDied
	}
	e.setKernel(KernelView{Status: "busy", Packages: pkgs.Packages, Go: pkgs.GoVersion})
	start := time.Now()
	size := 0
	resp, err := p.Exec(id, src, nums, func(stream, text string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if size > maxOutput {
			return
		}
		if size += len(text); size > maxOutput {
			text = "\n… output truncated\n"
		}
		o := Output{Stream: stream, Text: strings.ToValidUTF8(text, "�")}
		if n := len(st.Outputs); n > 0 && st.Outputs[n-1].Stream == stream {
			st.Outputs[n-1].Text += o.Text
		} else {
			st.Outputs = append(st.Outputs, o)
		}
		e.emitLocked(Event{Type: "output", Cell: id, Output: &o})
		if e.OnOutput != nil {
			e.OnOutput(o)
		}
	})
	e.mu.Lock()
	st.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	e.mu.Unlock()
	if err == nil {
		e.setKernel(KernelView{Status: "ready", Packages: pkgs.Packages, Go: pkgs.GoVersion})
	}
	return resp, err
}

// onlyDeclarations returns the cells run so far that come before and after id,
// if the session holds no variables and none of these contains statements.
func (e *Engine) onlyDeclarations(id string) (before, after []string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.vars) > 0 {
		return nil, nil, false
	}
	_, at := e.nb.Find(id)
	for _, x := range e.ranCells() {
		if x == id {
			continue
		}
		if in, err := cellsrc.Classify(e.states[x].ran); err == nil && in.HasStmt {
			return nil, nil, false
		}
		if _, i := e.nb.Find(x); i < at {
			before = append(before, x)
		} else {
			after = append(after, x)
		}
	}
	return before, after, true
}

// ranCells lists the code cells run in this session in document order, so that
// imports and declarations come before what uses them; e.mu is held.
func (e *Engine) ranCells() []string {
	var ids []string
	for _, c := range e.nb.Cells {
		if s := e.states[c.ID]; s != nil && s.Count > 0 && c.Kind == notebook.Code {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func (e *Engine) stopKernel() {
	e.mu.Lock()
	p := e.proc
	e.proc = nil
	e.vars, e.types = nil, nil
	for _, s := range e.states {
		s.Count = 0
	}
	e.mu.Unlock()
	if p != nil {
		p.Stop()
	}
}

// refreshVars updates the variable list; the caller holds e.run.
func (e *Engine) refreshVars() {
	e.mu.Lock()
	p, nums := e.proc, e.numbering()
	e.mu.Unlock()
	if p == nil {
		return
	}
	resp, err := p.Variables(nums)
	if err != nil {
		return
	}
	e.mu.Lock()
	e.vars, e.types = resp.Variables, resp.Types
	e.emitLocked(Event{Type: "variables"})
	e.mu.Unlock()
}

// Variables returns the live variables and types as of the last run.
func (e *Engine) Variables() ([]proto.Variable, []proto.Type) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]proto.Variable{}, e.vars...), append([]proto.Type{}, e.types...)
}

// runList runs cells in order. With strict it stops at the first failure,
// otherwise only where a cell fails that succeeded when it was last run.
func (e *Engine) runList(ctx context.Context, ids []string, strict bool) (ok bool) {
	failedBefore := map[string]bool{}
	e.mu.Lock()
	for _, id := range ids {
		s := e.states[id]
		failedBefore[id] = s != nil && s.Error != nil
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.setStatus(id, "queued")
	}
	ok = true
	for _, id := range ids {
		if !ok || ctx.Err() != nil {
			e.mu.Lock()
			if s := e.states[id]; s != nil && s.Status == "queued" {
				s.Status = ""
				e.emitCell(id)
			}
			e.mu.Unlock()
			continue
		}
		ok = e.runCell(ctx, id) || !strict && failedBefore[id]
	}
	return ok && ctx.Err() == nil
}

// RunAll executes all code cells in order, stopping at the first failure.
func (e *Engine) RunAll(ctx context.Context) bool {
	e.run.Lock()
	defer e.run.Unlock()
	e.mu.Lock()
	var ids []string
	for _, c := range e.nb.Cells {
		if c.Kind == notebook.Code && strings.TrimSpace(c.Source) != "" {
			ids = append(ids, c.ID)
		}
	}
	e.mu.Unlock()
	return e.runList(ctx, ids, true)
}

// RunStale re-runs the cells that created stale variables, in order.
func (e *Engine) RunStale(ctx context.Context) ([]string, bool) {
	e.run.Lock()
	defer e.run.Unlock()
	e.mu.Lock()
	want := map[string]bool{}
	for _, v := range e.vars {
		if v.Stale {
			want[v.CellID] = true
		}
	}
	ids := []string{}
	for _, c := range e.nb.Cells {
		if want[c.ID] {
			ids = append(ids, c.ID)
		}
	}
	e.mu.Unlock()
	return ids, e.runList(ctx, ids, true)
}

// Restart replaces the kernel with a fresh one. A running cell is ended.
func (e *Engine) Restart(ctx context.Context) error {
	e.mu.Lock()
	p := e.proc
	e.mu.Unlock()
	if p != nil && !e.run.TryLock() {
		p.Kill()
		e.run.Lock()
	} else if p == nil {
		e.run.Lock()
	}
	defer e.run.Unlock()
	e.stopKernel()
	e.emit(Event{Type: "variables"})
	err := e.ensure(ctx)
	e.emit(Event{Type: "notebook"})
	return err
}

// SyncDeps brings the kernel up to date with the notebook's imports: missing
// modules are added to go.mod with go get, the kernel is rebuilt, and with
// replay the cells that had run are run again, as a new process starts empty.
func (e *Engine) SyncDeps(ctx context.Context, replay bool) (rebuilt bool, err error) {
	e.run.Lock()
	defer e.run.Unlock()
	e.setKernel(KernelView{Status: "building", Message: "syncing dependencies"})
	fail := func(err error) (bool, error) {
		e.setKernel(KernelView{Status: "error", Message: err.Error()})
		return false, err
	}
	imports := e.imports()
	if err := e.b.Sync(ctx, imports); err != nil {
		return fail(err)
	}
	k, err := e.b.Build(ctx, imports)
	if err != nil {
		return fail(err)
	}
	e.mu.Lock()
	same := e.proc != nil && e.proc.Alive() && e.kernel != nil && e.kernel.Key == k.Key
	ids := e.ranCells()
	e.mu.Unlock()
	if same {
		e.setKernel(KernelView{Status: "ready", Packages: k.Info.Packages, Go: k.Info.GoVersion})
		return false, nil
	}
	e.stopKernel()
	if err := e.ensure(ctx); err != nil {
		return true, err
	}
	if replay {
		e.runList(ctx, ids, false)
	}
	e.emit(Event{Type: "notebook"})
	return true, nil
}

// BuildResult is the outcome of go build.
type BuildResult struct {
	OK         bool    `json:"ok"`
	Output     string  `json:"output"`
	Binary     string  `json:"binary,omitempty"`
	DurationMS float64 `json:"duration_ms"`
}

// GoBuild compiles the notebook file with go build. With out == "" the binary
// is thrown away.
func (e *Engine) GoBuild(ctx context.Context, out string) (BuildResult, error) {
	keep := out != ""
	if !keep {
		dir, err := os.MkdirTemp("", "gonotebooks-")
		if err != nil {
			return BuildResult{}, err
		}
		defer os.RemoveAll(dir)
		out = filepath.Join(dir, "notebook")
	}
	start := time.Now()
	msg, err := e.b.GoBuild(ctx, out)
	r := BuildResult{OK: err == nil, Output: strings.TrimSpace(string(bytes.TrimSpace([]byte(msg)))), DurationMS: float64(time.Since(start).Milliseconds())}
	if err != nil && r.Output == "" {
		r.Output = err.Error()
	}
	if keep && r.OK {
		r.Binary = out
	}
	return r, nil
}
