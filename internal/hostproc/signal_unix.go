//go:build unix

//declscope:namespace process

package hostproc

import "syscall"

// processSignalByLinuxUnix holds the signals only Unix hosts have.
var processSignalByLinuxUnix = map[int32]syscall.Signal{
	10: syscall.SIGUSR1,
	12: syscall.SIGUSR2,
	13: syscall.SIGPIPE,
	17: syscall.SIGCHLD,
	18: syscall.SIGCONT,
	19: syscall.SIGSTOP,
	20: syscall.SIGTSTP,
}
