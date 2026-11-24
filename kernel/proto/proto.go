// Package proto defines the line-delimited JSON protocol spoken between the
// go-notebooks process and a kernel process.
package proto

// EndMark is written by the kernel to stdout and stderr when a request is
// complete, so the parent knows all output of the cell has been received.
const EndMark = "\x00\x01gonb-end:%d\x01\x00"

// Request is one call into the kernel.
type Request struct {
	ID     int            `json:"id"`
	Method string         `json:"method"` // exec | variables | ping
	CellID string         `json:"cell_id,omitempty"`
	Source string         `json:"source,omitempty"`
	Cells  map[string]int `json:"cells,omitempty"` // cell id -> number shown to the user
}

// Response answers the Request with the same ID.
type Response struct {
	ID        int        `json:"id"`
	Error     *CellError `json:"error,omitempty"`
	Result    string     `json:"result,omitempty"` // value of a trailing expression
	Notices   []string   `json:"notices,omitempty"`
	Variables []Variable `json:"variables,omitempty"`
	Types     []Type     `json:"types,omitempty"`
}

// Error kinds.
const (
	ErrSyntax   = "syntax"
	ErrCompile  = "compile"
	ErrStale    = "stale"
	ErrPanic    = "panic"
	ErrNeedSync = "needs_sync"
	ErrInterp   = "interpreter"
	ErrKernel   = "kernel_died"
)

// CellError is a non-fatal failure of one cell.
type CellError struct {
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Line    int      `json:"line,omitempty"`
	Stale   []Stale  `json:"stale,omitempty"`   // for ErrStale
	Imports []string `json:"imports,omitempty"` // for ErrNeedSync
}

// Stale says why a symbol can no longer be used and how to refresh it.
type Stale struct {
	Name    string   `json:"name"`
	CellID  string   `json:"cell_id,omitempty"`
	Cell    int      `json:"cell,omitempty"`
	Reason  string   `json:"reason"`
	Changes []Change `json:"changes,omitempty"`
}

// Change is one version step of a named type.
type Change struct {
	Type    string   `json:"type"`
	From    int      `json:"from"`
	To      int      `json:"to"`
	Details []string `json:"details"`
}

// Variable is one live variable.
type Variable struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Value    string   `json:"value"`
	CellID   string   `json:"cell_id"`
	Versions []string `json:"versions,omitempty"` // e.g. "User v2"
	Stale    bool     `json:"stale"`
	Reason   string   `json:"reason,omitempty"`
	Changes  []Change `json:"changes,omitempty"`
}

// Type is one tracked named type.
type Type struct {
	Name    string   `json:"name"`
	Version int      `json:"version"`
	CellID  string   `json:"cell_id"`
	Schema  string   `json:"schema"`
	Fields  []Field  `json:"fields,omitempty"`
	History []Change `json:"history,omitempty"`
}

// Field is one struct field.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Tag  string `json:"tag,omitempty"`
}

// Manifest describes a built kernel; it sits next to the kernel binary.
type Manifest struct {
	GoVersion string            `json:"go_version"`
	Packages  []string          `json:"packages"` // extracted third-party packages
	Std       []string          `json:"std"`      // importable standard packages
	Exports   map[string]string `json:"exports"`  // import path -> export data file
	Names     map[string]string `json:"names"`    // import path -> package name
}
