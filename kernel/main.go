package kernel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"github.com/traefik/yaegi/stdlib/unsafe"

	"go-notebooks/kernel/proto"
)

// Environment of a kernel process. Requests arrive on file descriptor 3 and
// responses leave on 4, so that stdin, stdout and stderr belong to cells.
const ManifestEnv = "GONOTEBOOKS_MANIFEST"

// Main runs a kernel process with the given extra symbol tables.
func Main(extra ...interp.Exports) {
	if err := serve(extra); err != nil {
		fmt.Fprintln(os.Stderr, "kernel:", err)
		os.Exit(1)
	}
}

func serve(extra []interp.Exports) error {
	b, err := os.ReadFile(os.Getenv(ManifestEnv))
	if err != nil {
		return fmt.Errorf("no manifest (this program is started by go-notebooks): %w", err)
	}
	var man proto.Manifest
	if err := json.Unmarshal(b, &man); err != nil {
		return err
	}
	os.Unsetenv(ManifestEnv)
	in, out := os.NewFile(3, "requests"), os.NewFile(4, "responses")
	if in == nil || out == nil {
		return fmt.Errorf("started without request pipes")
	}
	os.Args = os.Args[:1]
	s, err := NewSession(&man, os.Stdout, os.Stderr, append([]interp.Exports{stdlib.Symbols, unsafe.Symbols}, extra...)...)
	if err != nil {
		return err
	}
	defer s.Close()
	r := bufio.NewReaderSize(in, 1<<20)
	enc := json.NewEncoder(out)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil // parent closed the pipe
		}
		var req proto.Request
		if err := json.Unmarshal(line, &req); err != nil {
			return err
		}
		var resp proto.Response
		switch req.Method {
		case "ping":
		case "exec":
			resp = s.Exec(req.CellID, req.Source, req.Cells)
			flushC()
		case "variables":
			resp = s.Variables(req.Cells)
		default:
			resp.Error = &proto.CellError{Kind: proto.ErrKernel, Message: "unknown method " + req.Method}
		}
		resp.ID = req.ID
		end := fmt.Sprintf(proto.EndMark, req.ID)
		os.Stdout.WriteString(end)
		os.Stderr.WriteString(end)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}
