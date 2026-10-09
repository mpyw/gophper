//go:build windows

//declscope:namespace process

package hostproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// processShell runs `sh -c command`, which is how popen(), proc_open() and
// exec() start a command line. Windows has no /bin/sh. PHP inside is a Unix
// build, so escapeshellarg() quotes for sh, and libraries write sh syntax:
// a sh from PATH, such as Git for Windows' or MSYS', runs them as meant.
// Without one, cmd.exe runs the line, as native PHP on Windows does. Its
// quoting differs, so escapeshellarg() does not protect what it passes.
func processShell(path string, argv []string) (*exec.Cmd, bool) {
	if path != "/bin/sh" || len(argv) != 3 || argv[1] != "-c" {
		return nil, false
	}
	// A program: a sh.bat would hand the line to cmd.exe unchecked. By its
	// full name, so that a sh.cmd earlier in PATH hides no sh.exe after it.
	if sh, err := exec.LookPath("sh.exe"); err == nil && strings.EqualFold(filepath.Ext(sh), ".exe") {
		return &exec.Cmd{Path: sh, Args: []string{"sh", "-c", argv[2]}}, true
	}
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	return &exec.Cmd{
		Path:        comspec,
		Args:        []string{comspec, "/s", "/c", argv[2]},
		SysProcAttr: &syscall.SysProcAttr{CmdLine: comspec + ` /s /c "` + argv[2] + `"`},
	}, true
}
