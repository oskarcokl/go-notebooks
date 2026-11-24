// Command go-notebooks edits, runs and builds notebooks that are plain Go files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go-notebooks/internal/manager"
	"go-notebooks/internal/notebook"
	"go-notebooks/internal/server"
	"go-notebooks/kernel/proto"
)

const usage = `go-notebooks: interactive notebooks in plain Go files

usage:
  go-notebooks serve <file.go> [--port 8080] [--open]   edit and run in the browser
  go-notebooks run   <file.go> [--sync] [--quiet]       run all cells and print their output
  go-notebooks build <file.go> [-o binary]              compile with go build

A notebook is an ordinary Go file whose cells start with "// %%" or
"// %% [markdown]" comments.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	cmds := map[string]func(context.Context, []string, io.Writer, io.Writer) int{"serve": serve, "run": runNotebook, "build": build}
	cmd, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	return cmd(ctx, args[1:], stdout, stderr)
}

// parse accepts flags before and after the file name.
func parse(fs *flag.FlagSet, args []string, stderr io.Writer) (string, bool) {
	fs.SetOutput(stderr)
	if fs.Parse(args) != nil {
		return "", false
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, usage)
		return "", false
	}
	file := fs.Arg(0)
	if fs.Parse(fs.Args()[1:]) != nil {
		return "", false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return "", false
	}
	return file, true
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", 8080, "port to listen on (0 picks a free one)")
	open := fs.Bool("open", false, "open the notebook in the default browser")
	file, ok := parse(fs, args, stderr)
	if !ok {
		return 2
	}
	e, err := manager.Open(file, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer e.Close()
	// Cells are arbitrary code: listen on this machine only.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	url := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	fmt.Fprintf(stdout, "%s is at %s\n", file, url)
	srv := &http.Server{Handler: server.New(ctx, e), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if srv.Shutdown(c) != nil {
			srv.Close()
		}
	}()
	go e.Restart(ctx) // have the kernel ready when the first cell runs
	if *open {
		openBrowser(url)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func openBrowser(url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	exec.Command(cmd, url).Start()
}

func runNotebook(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sync := fs.Bool("sync", false, "add missing imports to go.mod with go get first")
	quiet := fs.Bool("quiet", false, "print only the output of cells")
	file, ok := parse(fs, args, stderr)
	if !ok {
		return 2
	}
	if _, err := os.Stat(file); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	log := stderr
	if *quiet {
		log = io.Discard
	}
	e, err := manager.Open(file, log)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer e.Close()
	if *sync {
		if _, err := e.SyncDeps(ctx, false); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}
	e.OnOutput = func(o manager.Output) {
		if o.Stream == "stderr" {
			io.WriteString(stderr, o.Text)
		} else {
			io.WriteString(stdout, o.Text)
		}
	}
	start := time.Now()
	failed, n := false, 0
	for _, c := range e.View().Cells {
		if c.Kind != notebook.Code || strings.TrimSpace(c.Source) == "" || ctx.Err() != nil {
			continue
		}
		if !*quiet {
			title := ""
			if c.Title != "" {
				title = " " + c.Title
			}
			fmt.Fprintf(stderr, "── cell #%d%s\n", c.Num, title)
		}
		finished := make(chan struct{})
		go func() { // Ctrl-C ends a running cell
			select {
			case <-ctx.Done():
				e.Close()
			case <-finished:
			}
		}()
		v, err := e.RunCell(ctx, c.ID, nil)
		close(finished)
		n++
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			failed = true
			break
		}
		if v.State.Result != "" {
			fmt.Fprintf(stdout, "=> %s\n", v.State.Result)
		}
		if err := v.State.Error; err != nil {
			fmt.Fprintf(stderr, "cell #%d%s: %s\n", c.Num, lineOf(err), err.Message)
			if err.Kind == proto.ErrNeedSync {
				fmt.Fprintf(stderr, "hint: go-notebooks run --sync %s\n", file)
			}
			failed = true
			break
		}
	}
	if failed || ctx.Err() != nil {
		return 1
	}
	if !*quiet {
		fmt.Fprintf(stderr, "── %d cells ok in %.2fs\n", n, time.Since(start).Seconds())
	}
	return 0
}

func lineOf(e *proto.CellError) string {
	if e.Line == 0 {
		return ""
	}
	return fmt.Sprintf(", line %d", e.Line)
}

func build(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	out := fs.String("o", "", "binary to write (default: the notebook's name)")
	file, ok := parse(fs, args, stderr)
	if !ok {
		return 2
	}
	if _, err := os.Stat(file); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	e, err := manager.Open(file, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *out == "" {
		*out = strings.TrimSuffix(filepath.Base(file), ".go")
	}
	abs, err := filepath.Abs(*out)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	res, err := e.GoBuild(ctx, abs)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if !res.OK {
		fmt.Fprintln(stderr, res.Output)
		return 1
	}
	fmt.Fprintf(stdout, "ok: built %s in %.1fs\n", *out, res.DurationMS/1000)
	return 0
}
