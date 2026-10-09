//go:build windows

//declscope:namespace process

package hostproc

import (
	"os"
	"path/filepath"
	"strings"
)

// processDefaultPath is the PATH without one: Windows has none to add.
const processDefaultPath = ""

// processPathValue returns the value of a PATH entry of an environment.
// Windows spells the key Path, and matches it without case.
func processPathValue(kv string) (string, bool) {
	if len(kv) < 5 || !strings.EqualFold(kv[:5], "PATH=") {
		return "", false
	}
	return kv[5:], true
}

// processExecutable finds path, or path with an extension of PATHEXT, as
// Windows runs a name. Windows reports no execute bits.
func processExecutable(path string) (string, bool) {
	candidates := []string{path}
	if filepath.Ext(path) == "" {
		exts := os.Getenv("PATHEXT")
		if exts == "" {
			exts = ".COM;.EXE;.BAT;.CMD"
		}
		candidates = nil
		for ext := range strings.SplitSeq(exts, ";") {
			if ext != "" {
				candidates = append(candidates, path+strings.ToLower(ext))
			}
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			return c, true
		}
	}
	return "", false
}
