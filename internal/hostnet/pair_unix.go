//go:build unix

//declscope:namespace socket

package hostnet

import (
	"net"
	"os"
	"syscall"
)

// socketPair returns the two ends of a host socketpair(2), so that either
// can be given to a child process.
func socketPair() (net.Conn, net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	a, err := socketPairConn(fds[0])
	if err != nil {
		// The original error wins.
		_ = syscall.Close(fds[1])
		return nil, nil, err
	}
	b, err := socketPairConn(fds[1])
	if err != nil {
		// The original error wins.
		_ = a.Close()
		return nil, nil, err
	}
	return a, b, nil
}

func socketPairConn(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "socketpair")
	// FileConn duplicates the fd, so this closes only the original.
	defer func() { _ = f.Close() }()
	return net.FileConn(f)
}
