package hostnet

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"

	"github.com/mpyw/gophper/internal/wasi"
)

// errnoFrom maps a Go network error to the WASI errno a POSIX call would set.
//
//declscope:shared // socket.go
func errnoFrom(err error) int32 {
	for _, m := range errnoBySyscall {
		if errors.Is(err, m.sys) {
			return m.wasi
		}
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, os.ErrDeadlineExceeded):
		return wasi.ETIMEDOUT
	case errors.Is(err, net.ErrClosed):
		return wasi.EBADF
	case errors.Is(err, context.Canceled):
		return wasi.EINTR
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return wasi.EHOSTUNREACH
	}
	return wasi.EIO
}

var errnoBySyscall = []struct {
	sys  syscall.Errno
	wasi int32
}{
	{syscall.ECONNREFUSED, wasi.ECONNREFUSED},
	{syscall.ECONNRESET, wasi.ECONNRESET},
	{syscall.ECONNABORTED, wasi.ECONNABORTED},
	{syscall.ETIMEDOUT, wasi.ETIMEDOUT},
	{syscall.EHOSTUNREACH, wasi.EHOSTUNREACH},
	{syscall.ENETUNREACH, wasi.ENETUNREACH},
	{syscall.EADDRINUSE, wasi.EADDRINUSE},
	{syscall.EADDRNOTAVAIL, wasi.EADDRNOTAVAIL},
	{syscall.EACCES, wasi.EACCES},
	{syscall.EPERM, wasi.EACCES},
	{syscall.EPIPE, wasi.EPIPE},
	{syscall.ENOENT, wasi.ENOENT},
	{syscall.EAFNOSUPPORT, wasi.EAFNOSUPPORT},
	{syscall.EINVAL, wasi.EINVAL},
	{syscall.EMSGSIZE, wasi.EMSGSIZE},
	{syscall.ENOTCONN, wasi.ENOTCONN},
	{syscall.EISCONN, wasi.EISCONN},
}
