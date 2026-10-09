//go:build !unix

//declscope:namespace process

package hostproc

import "syscall"

// processSignalByLinuxUnix is empty: only Unix hosts have these signals.
var processSignalByLinuxUnix = map[int32]syscall.Signal{}
