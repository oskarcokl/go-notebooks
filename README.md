# go-notebooks

Interactive notebooks that are plain `.go` files. Cells run in one live
in-memory session; the file itself always stays something `go build` accepts.

    go build -o bin/go-notebooks ./cmd/go-notebooks

    bin/go-notebooks serve examples/demo.go --port 8080   # edit and run in the browser
    bin/go-notebooks run   examples/sqlite/sqlite.go      # run all cells, print output
    bin/go-notebooks build examples/demo.go -o demo       # ordinary go build

Requires Go and a C compiler on the machine (macOS or Linux).

## File format

| Line | Meaning |
| --- | --- |
| `// %% [markdown]` | Markdown cell: the `//` lines that follow |
| `// %%` or `// %% Title` | code cell |

Cells above (or below) `func main` hold declarations; cells inside its body
hold statements. When a cell is edited it is moved to a place where its content
is legal Go (imports first, functions outside `main`, statements inside it),
and the file is gofmt'ed. A Go file without any markers opens as well.

## Interactive rules

- Unused variables and imports are fine; **Verify `go build`** reports them.
- Re-running `x := 1` updates `x`. Functions can be redefined; callers and
  methods see the new body.
- A cell's last expression is shown. A `defer` at cell level waits until the
  cell is re-run or the kernel stops.
- Editing a type makes a new version of it. Functions, methods and types that
  use it are updated automatically. Variables of the old version become
  *stale*: cells that use one fail with the change and the cell to re-run, all
  others keep working. Re-running that cell (or assigning the variable) clears
  it; *Re-run stale cells* does it for all.
- A panic ends the cell, also when it happens in a goroutine. `os.Exit` panics.
- *Restart* also stops a cell that never ends.

## How it works

- `internal/notebook`: parser, serializer and cell edits.
- `internal/builder`: finds the third-party and local packages a notebook uses,
  compiles them (`CGO_ENABLED=1`), extracts their symbols from the compiler's
  export data (so cgo needs nothing special), adds standard library symbols
  newer than Yaegi's, and links a **kernel** = Yaegi + stdlib + dependencies.
  Kernels are cached by go.mod, go.sum, toolchain and package contents in the
  user cache directory (`GO_NOTEBOOKS_CACHE`). The project is built through a
  separate modfile and an overlay: its files are not touched. Only *Sync deps*
  / `run --sync` changes `go.mod` (`go get`).
- `kernel`: the session. Each cell is checked with `go/types` together with all
  live declarations, which gives plain Go errors, variable types and
  dependencies. Then it is passed to Yaegi with small source edits: notebook
  variables are package-level, redefined names get a fresh internal name.
- `internal/manager`: kernel processes (JSON lines over pipes; stdout and
  stderr belong to the cells) and the notebook engine.
- `internal/server`: JSON API and the embedded web page. It listens on
  127.0.0.1 only and refuses requests from other sites, since cells are
  arbitrary code.

A dependency that is new to a running kernel needs a rebuilt kernel, which
starts empty; *Sync deps* re-runs the cells that had run.

## API

`GET /api/notebook` · `POST /api/cells` · `GET|PATCH|DELETE /api/cells/{id}` ·
`POST /api/cells/{id}/run` · `POST /api/cells/{id}/move` · `POST /api/run-all` ·
`POST /api/run-stale` · `GET /api/variables` · `POST /api/build` ·
`POST /api/kernel/restart` · `POST /api/kernel/sync-deps` · `GET /api/events` (SSE)

## Limits

Cells are interpreted by Yaegi, so some Go behaves differently until it is
compiled with `go build`:

- **Type assertions and type switches on `any` can be wrong** for notebook
  types: `any(1).(Celsius)` succeeds if `Celsius` is an int type, and a switch
  may miss a case. `errors.As` to a notebook error type does not work.
- Notebook types have no methods for compiled code except through interfaces
  Yaegi knows at the call; `%T` prints the underlying type, and `%+v` and JSON
  show unexported fields with an `X` prefix (`Xage`).
- Generic functions of compiled packages (`slices.Sort`, `maps.Keys`, …) are
  mostly missing; generic code in cells works for simple cases. Ranging over
  functions and dot imports are not supported.
- A variable cannot have a type that its package does not export.
- Interpreted loops are much slower than compiled ones.
- Workspaces (`go.work`) are ignored when building kernels.

Most of these show up as a cell error, but the first group is silent.
