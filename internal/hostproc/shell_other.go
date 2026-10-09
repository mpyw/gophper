//go:build !windows

//declscope:namespace process

package hostproc

import "os/exec"

// processShell leaves `sh -c` to the host's /bin/sh.
func processShell(string, []string) (*exec.Cmd, bool) { return nil, false }
