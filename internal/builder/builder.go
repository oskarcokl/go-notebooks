// Package builder makes the kernel binary of a project: Yaegi, the standard
// library and the project's compiled dependencies, including cgo packages.
package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/importer"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/traefik/yaegi/stdlib"
	"github.com/traefik/yaegi/stdlib/unsafe"

	gonotebooks "go-notebooks"
	"go-notebooks/kernel/proto"
)

// Kernel is a built kernel.
type Kernel struct {
	Binary   string
	Manifest string // path of the manifest file
	Info     proto.Manifest
	Key      string
	Cached   bool // it existed already
}

// MissingError says that imports cannot be resolved by the project's go.mod.
type MissingError struct {
	Imports []string
	NoMod   bool // there is no go.mod at all
}

func (e *MissingError) Error() string {
	msg := "cannot resolve " + strings.Join(e.Imports, ", ")
	if e.NoMod {
		return msg + ": no go.mod found (sync dependencies to create one)"
	}
	return msg + " with the project's go.mod (sync dependencies to add it)"
}

// Builder builds kernels for the notebook at File.
type Builder struct {
	File     string // absolute path of the notebook
	CacheDir string // default: go-notebooks in the user's cache directory
	Log      io.Writer
}

// New returns a builder for a notebook file.
func New(file string, log io.Writer) (*Builder, error) {
	file, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = io.Discard
	}
	dir := os.Getenv("GO_NOTEBOOKS_CACHE")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "go-notebooks")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	return &Builder{File: file, CacheDir: dir, Log: log}, nil
}

// Dir is the directory of the notebook; cells run in it.
func (b *Builder) Dir() string { return filepath.Dir(b.File) }

func goEnv(dir string, names ...string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"env"}, names...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"), nil
}

// ModDir returns the directory of the notebook's go.mod, "" if it has none.
func (b *Builder) ModDir() (string, error) {
	v, err := goEnv(b.Dir(), "GOMOD")
	if err != nil {
		return "", err
	}
	if v[0] == "" || v[0] == os.DevNull {
		return "", nil
	}
	return filepath.Dir(v[0]), nil
}

const modName = "go-notebooks"

var moduleRE = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)`)

// project is the module a kernel is built in.
type project struct {
	dir     string // module root
	path    string // module path
	modfile string // alternative go.mod, next to its go.sum
	env     []string
}

// setup prepares the kernel sources and a go.mod that adds Yaegi and the
// kernel to the project's own requirements; the project is not modified.
func (b *Builder) setup(modDir, work string) (*project, error) {
	src, err := b.writeSources()
	if err != nil {
		return nil, err
	}
	// Overlay paths must be spelled as the go command sees them.
	if modDir != "" {
		if modDir, err = filepath.EvalSymlinks(modDir); err != nil {
			return nil, err
		}
	}
	if work, err = filepath.EvalSymlinks(work); err != nil {
		return nil, err
	}
	p := &project{dir: modDir, modfile: filepath.Join(work, "go.mod")}
	p.env = append(os.Environ(), "CGO_ENABLED=1", "GOWORK=off", "GOFLAGS=")
	if v, err := goEnv(b.Dir(), "GOFLAGS"); err == nil && strings.Contains(v[0], "-tags") {
		p.env = append(p.env, "GOFLAGS="+onlyTags(v[0]))
	}
	var gomod, gosum []byte
	if modDir == "" {
		p.dir = work
		gomod = []byte("module notebook\n\ngo " + goVersion(work) + "\n")
	} else {
		if gomod, err = os.ReadFile(filepath.Join(modDir, "go.mod")); err != nil {
			return nil, err
		}
		gosum, err = os.ReadFile(filepath.Join(modDir, "go.sum"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if m := moduleRE.FindSubmatch(gomod); m != nil {
		p.path = string(m[1])
	} else {
		return nil, fmt.Errorf("%s: no module line", filepath.Join(modDir, "go.mod"))
	}
	if err := os.WriteFile(p.modfile, gomod, 0o644); err != nil {
		return nil, err
	}
	sum := append(append(bytes.TrimRight(gosum, "\n"), '\n'), gonotebooks.GoSum...)
	if err := os.WriteFile(filepath.Join(work, "go.sum"), bytes.TrimLeft(sum, "\n"), 0o644); err != nil {
		return nil, err
	}
	if p.path != modName {
		args := []string{"mod", "edit", "-require=" + modName + "@v0.0.0", "-replace=" + modName + "=" + src}
		if !bytes.Contains(gomod, []byte("github.com/traefik/yaegi ")) {
			args = append(args, "-require=github.com/traefik/yaegi@"+yaegiVersion())
		}
		cmd := exec.Command("go", append(args, p.modfile)...)
		cmd.Env = p.env
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("go mod edit: %v\n%s", err, out)
		}
	}
	return p, nil
}

func onlyTags(flags string) string {
	for _, f := range strings.Fields(flags) {
		if strings.HasPrefix(f, "-tags") {
			return f
		}
	}
	return ""
}

func goVersion(dir string) string {
	if v, err := goEnv(dir, "GOVERSION"); err == nil {
		if m := regexp.MustCompile(`go(\d+\.\d+)`).FindStringSubmatch(v[0]); m != nil {
			return m[1]
		}
	}
	return "1.22"
}

func yaegiVersion() string {
	m := regexp.MustCompile(`github.com/traefik/yaegi (\S+)`).FindSubmatch(gonotebooks.GoMod)
	return string(m[1])
}

// writeSources unpacks the embedded kernel sources into the cache.
func (b *Builder) writeSources() (string, error) {
	h := sha256.New()
	files := map[string][]byte{}
	err := fs.WalkDir(gonotebooks.Kernel, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := gonotebooks.Kernel.ReadFile(path)
		files[path] = data
		fmt.Fprintf(h, "%s %d\n%s", path, len(data), data)
		return err
	})
	if err != nil {
		return "", err
	}
	h.Write(gonotebooks.GoMod)
	dir := filepath.Join(b.CacheDir, "src", fmt.Sprintf("%x", h.Sum(nil))[:16])
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	os.Chmod(tmp, 0o755)
	files["go.mod"] = []byte("module " + modName + "\n\ngo 1.22\n\nrequire github.com/traefik/yaegi " + yaegiVersion() + "\n")
	for path, data := range files {
		f := filepath.Join(tmp, path)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(f, data, 0o644); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, e := os.Stat(filepath.Join(dir, "go.mod")); e != nil {
			return "", err
		}
	}
	return dir, nil
}

// goCmd runs the go command with the alternative modfile.
func (p *project) goCmd(ctx context.Context, sub string, args ...string) ([]byte, string, error) {
	full := append([]string{sub, "-modfile=" + p.modfile, "-mod=mod"}, args...)
	cmd := exec.CommandContext(ctx, "go", full...)
	cmd.Dir, cmd.Env = p.dir, p.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.String(), err
}

type listed struct {
	ImportPath, Name, Export string
	Standard, DepOnly        bool
	Incomplete               bool
	Error                    *struct{ Err string }
	Imports                  []string
	Module                   *struct {
		Path string
		Main bool
	}
}

func (p *project) list(ctx context.Context, args ...string) ([]listed, string, error) {
	out, stderr, err := p.goCmd(ctx, "list", append([]string{"-json=ImportPath,Name,Export,Standard,DepOnly,Incomplete,Error,Imports,Module"}, args...)...)
	var pkgs []listed
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var l listed
		if e := dec.Decode(&l); e != nil {
			return pkgs, stderr, e
		}
		pkgs = append(pkgs, l)
	}
	return pkgs, stderr, err
}

// projectDeps lists third-party packages worth linking into the kernel besides
// the notebook's imports: what the module's packages import, and the root
// packages of its direct requirements.
func (p *project) projectDeps(ctx context.Context, hasMod bool) []string {
	if !hasMod {
		return nil
	}
	set := map[string]bool{}
	pkgs, _, _ := p.list(ctx, "-e", "./...")
	for _, l := range pkgs {
		for _, i := range l.Imports {
			if isThirdParty(i) && !inModule(i, p.path) {
				set[i] = true
			}
		}
	}
	out, _, err := p.goCmd(ctx, "list", "-m", "-f", "{{if not (or .Indirect .Main)}}{{.Path}}{{end}}", "all")
	if err == nil {
		for _, m := range strings.Fields(string(out)) {
			if m != modName {
				set[m] = true
			}
		}
	}
	var deps []string
	for d := range set {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return deps
}

func isThirdParty(path string) bool {
	return strings.Contains(strings.SplitN(path, "/", 2)[0], ".")
}

func inModule(path, mod string) bool { return path == mod || strings.HasPrefix(path, mod+"/") }

func usable(l listed, modPath string) bool {
	if l.Error != nil || l.Incomplete || l.Name == "main" || l.Name == "" || l.Standard {
		return false
	}
	if strings.HasPrefix(l.ImportPath, "github.com/traefik/yaegi") || strings.HasPrefix(l.ImportPath, modName+"/kernel") {
		return false
	}
	// internal packages can only be imported from within their parent.
	if i := strings.Index("/"+l.ImportPath+"/", "/internal/"); i >= 0 {
		return inModule(l.ImportPath, modPath)
	}
	return true
}

// Build returns the kernel for the notebook's imports, building it if it is
// not in the cache.
func (b *Builder) Build(ctx context.Context, imports []string) (*Kernel, error) {
	modDir, err := b.ModDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(b.CacheDir, "work"), 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(filepath.Join(b.CacheDir, "work"), "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	p, err := b.setup(modDir, work)
	if err != nil {
		return nil, err
	}

	ver, err := goEnv(p.dir, "GOVERSION", "GOOS", "GOARCH", "GOFLAGS")
	if err != nil {
		return nil, err
	}
	src, err := b.writeSources()
	if err != nil {
		return nil, err
	}
	var third []string
	std := map[string]bool{}
	for _, i := range imports {
		if i == "C" || i == "unsafe" {
			continue
		}
		if isThirdParty(i) || inModule(i, p.path) {
			third = append(third, i)
		} else {
			std[i] = true
		}
	}
	sort.Strings(third)

	must := map[string]bool{}
	for _, i := range third {
		must[i] = true
	}
	candidates := append([]string{}, third...)
	for _, d := range p.projectDeps(ctx, modDir != "") {
		if !must[d] {
			candidates = append(candidates, d)
		}
	}
	// Imports must be provided by the project's own go.mod, or go build would
	// reject the notebook later.
	known := b.resolvable(ctx, modDir, candidates)
	missing := &MissingError{NoMod: modDir == ""}
	var found []string
	for _, c := range candidates {
		if known[c] {
			found = append(found, c)
		} else if must[c] {
			missing.Imports = append(missing.Imports, c)
		}
	}
	if len(missing.Imports) > 0 {
		return nil, missing
	}
	var deps []listed
	if len(found) > 0 {
		pkgs, stderr, err := p.list(ctx, append([]string{"-e"}, found...)...)
		if err != nil && len(pkgs) == 0 {
			return nil, fmt.Errorf("go list: %v\n%s", err, stderr)
		}
		for _, l := range pkgs {
			if usable(l, p.path) {
				deps = append(deps, l)
			} else if must[l.ImportPath] && !strings.HasPrefix(l.ImportPath, "github.com/traefik/yaegi") && !strings.HasPrefix(l.ImportPath, modName+"/kernel") {
				detail := "it cannot be imported by a notebook"
				if l.Error != nil {
					detail = l.Error.Err
				}
				return nil, fmt.Errorf("package %s: %s", l.ImportPath, detail)
			}
		}
	}

	// Compile the dependencies. Optional ones that fail are left out.
	listArgs := []string{"-export", "-deps", "std", "github.com/traefik/yaegi/interp"}
	var good []listed
	for _, l := range deps {
		if must[l.ImportPath] {
			good = append(good, l)
			continue
		}
		if _, _, err := p.list(ctx, "-export", l.ImportPath); err == nil {
			good = append(good, l)
		}
	}
	deps = good
	for _, l := range deps {
		listArgs = append(listArgs, l.ImportPath)
	}
	fmt.Fprintf(b.Log, "compiling %d project package(s)\n", len(deps))
	all, stderr, err := p.list(ctx, listArgs...)
	if err != nil {
		return nil, fmt.Errorf("compiling dependencies failed:\n%s", stderr)
	}
	man := proto.Manifest{GoVersion: ver[0], Exports: map[string]string{}, Names: map[string]string{}}
	var stdNames []string
	for _, l := range all {
		if l.Export != "" {
			man.Exports[l.ImportPath] = l.Export
		}
		man.Names[l.ImportPath] = l.Name
		if l.Standard && importableStd(l.ImportPath) && l.Export != "" {
			stdNames = append(stdNames, l.ImportPath)
		}
	}
	man.Std = stdNames
	for _, l := range deps {
		man.Packages = append(man.Packages, l.ImportPath)
	}
	sort.Strings(man.Packages)

	// Export files are content-addressed, so they also identify the sources of
	// local packages and replaced modules.
	h := sha256.New()
	fmt.Fprintln(h, "v1", ver, src, p.path, len(stdNames))
	for _, name := range []string{"go.mod", "go.sum"} {
		data, _ := os.ReadFile(filepath.Join(work, name))
		h.Write(data)
	}
	for _, pk := range man.Packages {
		fmt.Fprintln(h, pk, filepath.Base(man.Exports[pk]))
	}
	key := fmt.Sprintf("%x", h.Sum(nil))[:24]
	dir := filepath.Join(b.CacheDir, "kernels", key)
	k := &Kernel{Binary: filepath.Join(dir, "kernel"), Manifest: filepath.Join(dir, "manifest.json"), Info: man, Key: key}
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("go-notebooks kernels need a Unix system")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Always refresh the manifest: the go build cache may have moved files.
	data, _ := json.Marshal(man)
	if err := writeAtomic(k.Manifest, data, 0o644); err != nil {
		return nil, err
	}
	if _, err := os.Stat(k.Binary); err == nil {
		k.Cached = true
		return k, nil
	}
	if err := b.compile(ctx, p, work, k, &man); err != nil {
		return nil, err
	}
	return k, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func importableStd(path string) bool {
	for _, p := range []string{"internal", "vendor"} {
		if strings.Contains("/"+path+"/", "/"+p+"/") {
			return false
		}
	}
	switch path {
	case "plugin", "runtime/cgo", "runtime/race", "runtime/asan", "runtime/msan", "unsafe", "syscall/js":
		return false
	}
	return true
}

// compile generates symbol tables and builds the kernel binary.
func (b *Builder) compile(ctx context.Context, p *project, work string, k *Kernel, man *proto.Manifest) error {
	start := time.Now()
	if want := man.GoVersion; runtime.Version() != want {
		fmt.Fprintf(b.Log, "note: go-notebooks was built with %s, the project uses %s\n", runtime.Version(), want)
	}
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		f := man.Exports[path]
		if f == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	gen := filepath.Join(work, "gen")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return err
	}
	overlay := map[string]string{}
	// The main package exists only in the overlay, inside the project's module
	// so that it may import the module's internal packages.
	const mainDir = "gonotebooks_kernel_main"
	add := func(name, text string) error {
		f := filepath.Join(gen, name)
		overlay[filepath.Join(p.dir, mainDir, name)] = f
		return os.WriteFile(f, []byte(text), 0o644)
	}
	n := 0
	emit := func(path string, skip func(string) bool) error {
		pkg, err := imp.Import(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		n++
		if text := extract(pkg, skip); text != "" {
			return add(fmt.Sprintf("sym%d.go", n), text)
		}
		return add(fmt.Sprintf("sym%d.go", n), fmt.Sprintf("package main\n\nimport _ %q\n", path))
	}
	for _, path := range man.Packages {
		if err := emit(path, func(string) bool { return false }); err != nil {
			return err
		}
	}
	// Yaegi ships symbols for an older standard library: add what is missing.
	for _, path := range man.Std {
		name := man.Names[path]
		have := stdlib.Symbols[path+"/"+name]
		if have == nil {
			have = unsafe.Symbols[path+"/"+name]
		}
		if path == "syscall" {
			continue
		}
		if err := emit(path, func(sym string) bool { _, ok := have[sym]; return ok }); err != nil {
			return err
		}
	}
	if err := add("main.go", "package main\n\nimport (\n\t\"github.com/traefik/yaegi/interp\"\n\n\t\"go-notebooks/kernel\"\n)\n\nvar symbols = interp.Exports{}\n\nfunc main() { kernel.Main(symbols) }\n"); err != nil {
		return err
	}
	ov, _ := json.Marshal(map[string]any{"Replace": overlay})
	ovFile := filepath.Join(work, "overlay.json")
	if err := os.WriteFile(ovFile, ov, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(b.Log, "building kernel (%d packages from the project)\n", len(man.Packages))
	tmp := filepath.Join(work, "kernel")
	_, stderr, err := p.goCmd(ctx, "build", "-overlay="+ovFile, "-ldflags=-s -w", "-o", tmp, "./"+mainDir)
	if err != nil {
		return fmt.Errorf("building the kernel failed: %v\n%s", err, stderr)
	}
	if err := os.Rename(tmp, k.Binary); err != nil {
		return err
	}
	fmt.Fprintf(b.Log, "kernel ready in %.1fs\n", time.Since(start).Seconds())
	return nil
}

// Sync adds missing imports to the project's go.mod with go get, creating the
// go.mod if there is none. It modifies the project and may use the network.
func (b *Builder) Sync(ctx context.Context, imports []string) error {
	modDir, err := b.ModDir()
	if err != nil {
		return err
	}
	var third []string
	for _, i := range imports {
		if isThirdParty(i) {
			third = append(third, i)
		}
	}
	if len(third) == 0 {
		return nil
	}
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = b.Dir()
		out, err := cmd.CombinedOutput()
		b.Log.Write(out)
		if err != nil {
			return fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	if modDir == "" {
		if err := run("mod", "init", "notebook"); err != nil {
			return err
		}
	}
	var missing []string
	for _, i := range third {
		cmd := exec.CommandContext(ctx, "go", "list", "-find", i)
		cmd.Dir = b.Dir()
		if cmd.Run() != nil {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return run(append([]string{"get"}, missing...)...)
}

// resolvable reports which packages the project's go.mod provides.
func (b *Builder) resolvable(ctx context.Context, modDir string, paths []string) map[string]bool {
	out := map[string]bool{}
	if modDir == "" || len(paths) == 0 {
		return out
	}
	cmd := exec.CommandContext(ctx, "go", append([]string{"list", "-e", "-find", "-f", "{{if not .Error}}{{.ImportPath}}{{end}}"}, paths...)...)
	cmd.Dir = modDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	text, _ := cmd.Output()
	for _, p := range strings.Fields(string(text)) {
		out[p] = true
	}
	return out
}

// GoBuild compiles the notebook itself with the ordinary go command.
func (b *Builder) GoBuild(ctx context.Context, out string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, filepath.Base(b.File))
	cmd.Dir = b.Dir()
	msg, err := cmd.CombinedOutput()
	return string(msg), err
}
