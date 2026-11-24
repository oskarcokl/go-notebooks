package kernel

import (
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"

	"go-notebooks/kernel/proto"
)

// newImporter type-checks imports from the compiler's export data listed in
// the manifest, which also covers cgo packages.
func newImporter(man *proto.Manifest) types.Importer {
	return importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		f := man.Exports[path]
		if f == "" {
			return nil, fmt.Errorf("package %s is not part of this kernel", path)
		}
		return os.Open(f)
	})
}
