package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cli(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsage(t *testing.T) {
	if code, _, _ := cli(t); code != 2 {
		t.Error(code)
	}
	if code, out, _ := cli(t, "help"); code != 0 || !strings.Contains(out, "serve") {
		t.Error(code, out)
	}
	if code, _, e := cli(t, "nope"); code != 2 || !strings.Contains(e, "unknown command") {
		t.Error(code, e)
	}
	if code, _, _ := cli(t, "run", "missing.go"); code != 1 {
		t.Error(code)
	}
}

// Each example must print the same through the kernel and as a binary.
func TestExamplesRunAndBuild(t *testing.T) {
	for _, file := range []string{"demo.go", "sqlite/sqlite.go"} {
		path := filepath.Join("..", "..", "examples", file)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code, kernelOut, errOut := cli(t, "run", path, "--quiet")
		if code != 0 {
			t.Fatalf("%s: run: %s", file, errOut)
		}
		bin := filepath.Join(t.TempDir(), "example")
		if code, _, errOut := cli(t, "build", path, "-o", bin); code != 0 {
			t.Fatalf("%s: build: %s", file, errOut)
		}
		out, err := exec.Command(bin).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != kernelOut {
			t.Errorf("%s: kernel and binary differ:\n%s\n---\n%s", file, kernelOut, out)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Errorf("%s was modified", file)
		}
	}
}

func TestRunStopsAtFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nb.go")
	src := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\t// %%\n\tx := 1\n\tfmt.Println(x / (x - 1))\n\n\t// %%\n\tfmt.Println(\"never\")\n}\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := cli(t, "run", file)
	if code != 1 || out != "" || !strings.Contains(errOut, "cell #2, line 2: panic: integer divide by zero") {
		t.Errorf("%d %q %q", code, out, errOut)
	}
	os.WriteFile(file, []byte("package main\n\nfunc main() {\n\t// %%\n\tx := 1\n}\n"), 0o644)
	if code, _, e := cli(t, "run", file); code != 0 {
		t.Errorf("unused variable failed interactively: %s", e)
	}
	if code, _, e := cli(t, "build", file); code != 1 || !strings.Contains(e, "declared and not used: x") {
		t.Errorf("%d %q", code, e)
	}
}
