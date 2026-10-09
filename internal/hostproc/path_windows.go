//go:build windows

//declscope:namespace process

package hostproc

import (
	"os"
	"os/exec"
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
	// The name as it is if it has an extension, then with each of PATHEXT,
	// as os/exec.LookPath does: foo.bat.exe is found by foo.bat.
	var candidates []string
	if filepath.Ext(path) != "" {
		candidates = append(candidates, path)
	}
	exts := os.Getenv("PATHEXT")
	if exts == "" {
		exts = ".COM;.EXE;.BAT;.CMD"
	}
	for ext := range strings.SplitSeq(exts, ";") {
		if ext != "" {
			candidates = append(candidates, path+strings.ToLower(ext))
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			return c, true
		}
	}
	return "", false
}

// processBatchSafe reports whether Windows can start name with args as
// they are. A batch file runs through cmd.exe, which reads its command line
// otherwise than the program arguments os/exec quotes for: & in an argument
// would start another command (BatBadBut, CVE-2024-1874 in PHP). Such an
// argument is refused.
func processBatchSafe(name string, args []string) bool {
	// Windows drops trailing dots and spaces: run.bat. is run.bat. Only a
	// program, not a script cmd.exe would read, gets its arguments as is.
	switch strings.ToLower(filepath.Ext(strings.TrimRight(name, ". "))) {
	case ".exe", ".com":
		return true
	}
	for _, a := range args {
		if strings.ContainsAny(a, "\"&|<>^%!\r\n") {
			return false
		}
	}
	return true
}

// processFinalPath is the file Windows runs for path: os/exec.Cmd.Start
// would try the extensions of PATHEXT itself, after any check of the name.
func processFinalPath(path string) (string, error) {
	return exec.LookPath(path)
}
