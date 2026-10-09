// Package wasi holds what every package of host functions shares with the
// guest: WASI's errno values, and the PHP instance a host function serves.
package wasi

import "context"

// WASI errno values, as wasi-libc numbers them (__errno_values.h). A host
// function returns them, and the guest sets errno from them.
const (
	EACCES          int32 = 2
	EADDRINUSE      int32 = 3
	EADDRNOTAVAIL   int32 = 4
	EAFNOSUPPORT    int32 = 5
	EAGAIN          int32 = 6
	EALREADY        int32 = 7
	EBADF           int32 = 8
	ECHILD          int32 = 12
	ECONNABORTED    int32 = 13
	ECONNREFUSED    int32 = 14
	ECONNRESET      int32 = 15
	EDESTADDRREQ    int32 = 17
	EHOSTUNREACH    int32 = 23
	EINPROGRESS     int32 = 26
	EINTR           int32 = 27
	EINVAL          int32 = 28
	EIO             int32 = 29
	EISCONN         int32 = 30
	EMSGSIZE        int32 = 35
	ENETUNREACH     int32 = 40
	ENOENT          int32 = 44
	ENOEXEC         int32 = 45
	ENOTCONN        int32 = 53
	ENOTSUP         int32 = 58
	EPERM           int32 = 63
	EPIPE           int32 = 64
	EPROTONOSUPPORT int32 = 66
	EROFS           int32 = 69
	ESRCH           int32 = 71
	ETIMEDOUT       int32 = 73
)

// Run is what host functions need from the PHP instance calling them.
type Run interface {
	// Context is the run's context. A blocking call returns EIO once it
	// is done.
	Context() context.Context
	// Interruption is closed by the next interrupt, such as a timeout
	// firing. A blocking call cut short by it returns EINTR, as a signal
	// would make it.
	Interruption() <-chan struct{}
}
