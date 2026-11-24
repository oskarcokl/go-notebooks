// Package gonotebooks embeds the kernel sources, from which a kernel binary is
// built for each project.
package gonotebooks

import "embed"

// Kernel holds the sources of the kernel packages.
//
//go:embed kernel
var Kernel embed.FS

// GoMod and GoSum are the module files of go-notebooks, for the versions and
// checksums of the kernel's dependencies.
//
//go:embed go.mod
var GoMod []byte

//go:embed go.sum
var GoSum []byte
