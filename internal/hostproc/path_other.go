//go:build !windows

//declscope:namespace process

package hostproc

import (
	"os"
	"strings"
)

// processDefaultPath is the PATH a shell would use without one.
const processDefaultPath = "/usr/local/bin:/usr/bin:/bin"

// processPathValue returns the value of a PATH entry of an environment.
func processPathValue(kv string) (string, bool) {
	return strings.CutPrefix(kv, "PATH=")
}

// processExecutable reports whether path is a file this user may run.
func processExecutable(path string) (string, bool) {
	st, err := os.Stat(path)
	return path, err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// processBatchSafe reports whether name can start with args. Only Windows
// has batch files, which cmd.exe runs.
func processBatchSafe(string, []string) bool { return true }

// processFinalPath is the file the host runs for path: path itself.
func processFinalPath(path string) (string, error) { return path, nil }
