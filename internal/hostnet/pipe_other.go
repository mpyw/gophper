//go:build !windows

//declscope:namespace socket

package hostnet

import "os"

// socketPipeClose closes a pipe's read end. The poller wakes the read in
// progress, which then ends.
func socketPipeClose(f *os.File, _ <-chan struct{}) error {
	return f.Close()
}
