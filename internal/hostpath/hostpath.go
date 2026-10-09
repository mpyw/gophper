//declscope:namespace hostpath

// Package hostpath maps host paths to the paths PHP sees, and back.
//
// PHP runs in wasm, where every path is a POSIX one. On Unix, a host path
// is the same inside PHP. On Windows, a drive appears as a directory at
// the root, as in Git Bash and MSYS: D:\app\index.php is /d/app/index.php.
package hostpath

import (
	"path"
	"strings"
)

// Root is a host directory mounted at a path inside PHP, to reach every
// host path: "/" on Unix, and each drive on Windows.
type Root struct {
	Host, Guest string
}

// CleanGuest cleans a path PHP gave, before it is mapped to the host. It
// reports false for a path that is not absolute, or that holds what the
// host would read as a separator or a drive: "\\" or ":" on Windows. A
// path with ".." left in it could leave its mount, so none is.
func CleanGuest(guest string) (string, bool) {
	if !strings.HasPrefix(guest, "/") || hostpathForeignSeparator(guest) {
		return "", false
	}
	return path.Clean(guest), true
}
