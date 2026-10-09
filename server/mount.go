package server

import (
	"path/filepath"
	"strings"
)

// Mount is a host directory PHP may access.
type Mount struct {
	// Dir is an absolute host path. PHP sees it at the same path, or on
	// Windows at /<drive>/..., as package hostpath maps it.
	Dir      string
	ReadOnly bool
}

// contains reports whether a host path is inside m.
//
//declscope:shared // pool.go checks scripts and the document root against the mounts
func (m Mount) contains(path string) bool {
	rel, err := filepath.Rel(m.Dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
