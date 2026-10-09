//go:build unix

//declscope:namespace main

package main

import (
	"os"
	"syscall"
)

// phpSignals are the signals "gophper php" passes to PHP.
var phpSignals = []os.Signal{
	syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
	syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH,
}
