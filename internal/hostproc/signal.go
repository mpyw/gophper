//declscope:namespace process

package hostproc

import (
	"os"
	"syscall"
)

// Signals are numbered as on Linux, which wasi-libc follows, and mapped to
// the host's numbers.
var processSignalByLinux = map[int32]syscall.Signal{
	1:  syscall.SIGHUP,
	2:  syscall.SIGINT,
	3:  syscall.SIGQUIT,
	6:  syscall.SIGABRT,
	9:  syscall.SIGKILL,
	14: syscall.SIGALRM,
	15: syscall.SIGTERM,
}

// processHostSignal returns the host signal for a Linux signal number.
func processHostSignal(sig int32) (os.Signal, bool) {
	if s, ok := processSignalByLinux[sig]; ok {
		return s, true
	}
	if s, ok := processSignalByLinuxUnix[sig]; ok {
		return s, true
	}
	return nil, false
}

// ProcessSignalNumber returns the Linux number of a host signal, as the
// guest numbers signals.
func ProcessSignalNumber(sig os.Signal) (int32, bool) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return 0, false
	}
	for n, hs := range processSignalByLinux {
		if hs == s {
			return n, true
		}
	}
	for n, hs := range processSignalByLinuxUnix {
		if hs == s {
			return n, true
		}
	}
	return 0, false
}

// processExitSignal returns the Linux number of the signal that ended a process.
func processExitSignal(ps *os.ProcessState) (int32, bool) {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	for n, s := range processSignalByLinux {
		if s == ws.Signal() {
			return n, true
		}
	}
	for n, s := range processSignalByLinuxUnix {
		if s == ws.Signal() {
			return n, true
		}
	}
	return int32(ws.Signal()), true
}
