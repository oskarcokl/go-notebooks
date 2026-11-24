// Package manager runs kernel processes.
package manager

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go-notebooks/internal/builder"
	"go-notebooks/kernel"
	"go-notebooks/kernel/proto"
)

// ErrDied is returned when the kernel process ends during a call.
var ErrDied = errors.New("the kernel process ended")

// Process is one running kernel.
type Process struct {
	cmd    *exec.Cmd
	in     *os.File
	out    *bufio.Reader
	sinkMu sync.Mutex
	sink   func(stream, text string)

	mu      sync.Mutex // one call at a time
	nextID  int
	marks   [2]chan int   // end marks seen on stdout and stderr
	done    chan struct{} // closed when the process has exited
	waitErr error
}

// Start launches a kernel in dir.
func Start(k *builder.Kernel, dir string) (*Process, error) {
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	respR, respW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		return nil, err
	}
	p := &Process{in: reqW, out: bufio.NewReaderSize(respR, 1<<20), done: make(chan struct{})}
	p.cmd = exec.Command(k.Binary)
	p.cmd.Dir = dir
	p.cmd.Env = append(os.Environ(), kernel.ManifestEnv+"="+k.Manifest)
	p.cmd.ExtraFiles = []*os.File{reqR, respW}
	setProcGroup(p.cmd)
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	err = p.cmd.Start()
	reqR.Close()
	respW.Close()
	if err != nil {
		reqW.Close()
		respR.Close()
		return nil, fmt.Errorf("starting kernel: %w", err)
	}
	p.marks = [2]chan int{make(chan int, 16), make(chan int, 16)}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pump(stdout, "stdout", p.marks[0]) }()
	go func() { defer wg.Done(); p.pump(stderr, "stderr", p.marks[1]) }()
	go func() {
		wg.Wait()
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	if _, err := p.call(proto.Request{Method: "ping"}, nil); err != nil {
		p.Kill()
		return nil, fmt.Errorf("kernel did not start: %w", err)
	}
	return p, nil
}

var (
	markStart = []byte(strings.SplitN(proto.EndMark, "%d", 2)[0])
	markEnd   = []byte(strings.SplitN(proto.EndMark, "%d", 2)[1])
)

// pump copies cell output to the sink and reports end marks.
func (p *Process) pump(r io.Reader, stream string, marks chan<- int) {
	defer close(marks)
	var buf []byte
	tmp := make([]byte, 32<<10)
	emit := func(b []byte) {
		if len(b) == 0 {
			return
		}
		p.sinkMu.Lock()
		if p.sink != nil {
			p.sink(stream, string(b))
		}
		p.sinkMu.Unlock()
	}
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		for {
			i := bytes.Index(buf, markStart)
			if i < 0 {
				break
			}
			j := bytes.Index(buf[i:], markEnd)
			if j < 0 {
				break
			}
			emit(buf[:i])
			id := 0
			fmt.Sscan(string(buf[i+len(markStart):i+j]), &id)
			marks <- id
			buf = append([]byte{}, buf[i+j+len(markEnd):]...)
		}
		// Hold back what may be the beginning of a mark.
		keep := 0
		if i := bytes.Index(buf, markStart); i >= 0 {
			keep = len(buf) - i
		} else if i := bytes.LastIndexByte(buf, markStart[0]); i >= 0 && bytes.HasPrefix(markStart, buf[i:]) {
			keep = len(buf) - i
		}
		emit(buf[:len(buf)-keep])
		buf = append([]byte{}, buf[len(buf)-keep:]...)
		if err != nil {
			emit(buf)
			return
		}
	}
}

func (p *Process) call(req proto.Request, sink func(stream, text string)) (proto.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var resp proto.Response
	p.nextID++
	req.ID = p.nextID
	if sink != nil {
		p.setSink(sink) // stays in place for later output of goroutines
	}
	b, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		return resp, p.died()
	}
	line, err := p.out.ReadBytes('\n')
	if err != nil {
		return resp, p.died()
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return resp, err
	}
	for _, c := range p.marks {
		for id := range c {
			if id == req.ID {
				break
			}
		}
	}
	return resp, nil
}

func (p *Process) setSink(f func(stream, text string)) {
	p.sinkMu.Lock()
	p.sink = f
	p.sinkMu.Unlock()
}

// died waits for the last output of a dead kernel.
func (p *Process) died() error {
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
	if p.Alive() {
		return ErrDied
	}
	if p.waitErr != nil {
		return fmt.Errorf("%w: %v", ErrDied, p.waitErr)
	}
	return ErrDied
}

// Exec runs a cell; sink receives its output as it appears.
func (p *Process) Exec(cell, source string, cells map[string]int, sink func(stream, text string)) (proto.Response, error) {
	return p.call(proto.Request{Method: "exec", CellID: cell, Source: source, Cells: cells}, sink)
}

// Variables returns the live variables and types.
func (p *Process) Variables(cells map[string]int) (proto.Response, error) {
	return p.call(proto.Request{Method: "variables", Cells: cells}, nil)
}

// Alive reports whether the process still runs.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Stop ends the kernel, letting idle ones run their pending defers.
func (p *Process) Stop() {
	if !p.mu.TryLock() { // a cell is running
		p.Kill()
		return
	}
	p.in.Close()
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		p.Kill()
	}
}

// Kill ends the kernel at once, also while a cell runs.
func (p *Process) Kill() {
	killGroup(p.cmd)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}
