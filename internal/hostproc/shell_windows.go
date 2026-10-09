//go:build windows

//declscope:namespace process

package hostproc

import (
	"os"
	"os/exec"
	"syscall"
)

// processShell runs `sh -c command`, which is how popen(), proc_open() and
// exec() start a command line, with cmd.exe: Windows has no /bin/sh.
// Native PHP on Windows runs them the same way. The command line goes
// through as written, since cmd.exe parses its own quotes.
func processShell(path string, argv []string) (*exec.Cmd, bool) {
	if path != "/bin/sh" || len(argv) != 3 || argv[1] != "-c" {
		return nil, false
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
