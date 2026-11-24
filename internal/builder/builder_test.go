package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildWithCgoDependency(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a kernel")
	}
	// The repository's own module requires go-sqlite3.
	b, err := New(filepath.Join("..", "..", "examples", "sqlite", "sqlite.go"), testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	k, err := b.Build(context.Background(), []string{"fmt", "database/sql", "github.com/mattn/go-sqlite3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(k.Binary); err != nil {
		t.Fatal(err)
	}
	again, err := b.Build(context.Background(), []string{"fmt", "database/sql", "github.com/mattn/go-sqlite3"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cached || again.Key != k.Key {
		t.Errorf("second build was not served from the cache")
	}
}

func TestMissingImport(t *testing.T) {
	dir := t.TempDir()
	b, err := New(filepath.Join(dir, "nb.go"), testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Build(context.Background(), []string{"example.com/does/not/exist"})
	m, ok := err.(*MissingError)
	if !ok || !m.NoMod || len(m.Imports) != 1 {
		t.Errorf("err = %v", err)
	}
}

// A module that happens to be in the module cache but not in go.mod is
// missing, too: go build would not find it.
func TestImportNotInGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := New(filepath.Join(dir, "nb.go"), testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Build(context.Background(), []string{"github.com/mattn/go-sqlite3"})
	if m, ok := err.(*MissingError); !ok || m.NoMod {
		t.Errorf("err = %v", err)
	}
	// An old go directive does not get in the way of the kernel.
	if _, err := b.Build(context.Background(), []string{"fmt"}); err != nil {
		t.Error(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "go.mod")); string(data) != "module m\n\ngo 1.22\n" {
		t.Errorf("go.mod was modified: %s", data)
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) { l.t.Logf("%s", p); return len(p), nil }
