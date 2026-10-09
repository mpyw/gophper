//go:build windows

//declscope:namespace hostpath

package hostpath

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Guest returns the path inside PHP for a host path: D:\a\b is /d/a/b. A
// path without a drive letter, such as a relative or a UNC one, keeps its
// slashes turned around only.
func Guest(host string) string {
	// C:foo is relative to the current directory on C:, so it is left alone.
	if vol := filepath.VolumeName(host); len(vol) == 2 && vol[1] == ':' && (len(host) == 2 || os.IsPathSeparator(host[2])) {
		return path.Clean("/" + strings.ToLower(vol[:1]) + "/" + filepath.ToSlash(host[2:]))
	}
	return filepath.ToSlash(host)
}

// Host returns the host path for a path inside PHP: /d/a/b is D:\a\b.
// It reports false for a path outside every drive, such as "/" itself.
func Host(guest string) (string, bool) {
	guest, ok := CleanGuest(guest)
	if !ok {
		return "", false
	}
	// Lowercase only, as Roots mounts them: /C is no drive inside PHP.
	if len(guest) < 2 || guest[0] != '/' || guest[1] < 'a' || guest[1] > 'z' || (len(guest) > 2 && guest[2] != '/') {
		return "", false
	}
	rest := strings.TrimPrefix(guest[2:], "/")
	return strings.ToUpper(guest[1:2]) + `:\` + filepath.FromSlash(rest), true
}

// Roots returns a mount for each drive that exists, at /c, /d and so on.
func Roots() []Root {
	var roots []Root
	for l := 'a'; l <= 'z'; l++ {
		host := strings.ToUpper(string(l)) + `:\`
		if _, err := os.Stat(host); err == nil {
			roots = append(roots, Root{Host: host, Guest: "/" + string(l)})
		}
	}
	return roots
}
