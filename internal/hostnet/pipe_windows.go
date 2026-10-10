//go:build windows

//declscope:namespace socket

package hostnet

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// socketPipeCancelEvery is how often socketPipeClose cancels the read.
var socketPipeCancelEvery = 10 * time.Millisecond

// socketPipeClose closes a pipe's read end once its read ahead has ended.
// Windows reads a pipe synchronously. Close cancels the read in progress
// with CancelIoEx, once, and then waits for it. A read that starts just
// after that cancel blocks, and Close with it, until the writer writes or
// goes. So the read is cancelled until it ends, and only then closed.
// Control keeps the handle open while CancelIoEx uses it.
func socketPipeClose(f *os.File, done <-chan struct{}) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	tick := time.NewTicker(socketPipeCancelEvery)
	defer tick.Stop()
	for {
		// ERROR_NOT_FOUND when no read is pending yet, or any more.
		_ = rc.Control(func(fd uintptr) { _ = windows.CancelIoEx(windows.Handle(fd), nil) })
		select {
		case <-done:
			return f.Close()
		case <-tick.C:
		}
	}
}
