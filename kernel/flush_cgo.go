//go:build cgo

package kernel

// #include <stdio.h>
import "C"

// flushC makes output printed by C code appear with the cell that caused it.
func flushC() { C.fflush(nil) }
