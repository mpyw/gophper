//go:build windows

//declscope:namespace errno

package hostnet

import (
	"syscall"

	"github.com/mpyw/gophper/internal/wasi"
)

// errnoByPlatform holds Windows' own numbers. Winsock reports WSAE*
// codes, not the POSIX values syscall invents for them, and an overlapped
// connect reports Win32 errors.
var errnoByPlatform = []errnoMapping{
	{syscall.Errno(10013), wasi.EACCES},        // WSAEACCES
	{syscall.Errno(10022), wasi.EINVAL},        // WSAEINVAL
	{syscall.Errno(10040), wasi.EMSGSIZE},      // WSAEMSGSIZE
	{syscall.Errno(10047), wasi.EAFNOSUPPORT},  // WSAEAFNOSUPPORT
	{syscall.Errno(10048), wasi.EADDRINUSE},    // WSAEADDRINUSE
	{syscall.Errno(10049), wasi.EADDRNOTAVAIL}, // WSAEADDRNOTAVAIL
	{syscall.Errno(10051), wasi.ENETUNREACH},   // WSAENETUNREACH
	{syscall.Errno(10053), wasi.ECONNABORTED},  // WSAECONNABORTED
	{syscall.Errno(10054), wasi.ECONNRESET},    // WSAECONNRESET
	{syscall.Errno(10056), wasi.EISCONN},       // WSAEISCONN
	{syscall.Errno(10057), wasi.ENOTCONN},      // WSAENOTCONN
	{syscall.Errno(10060), wasi.ETIMEDOUT},     // WSAETIMEDOUT
	{syscall.Errno(10061), wasi.ECONNREFUSED},  // WSAECONNREFUSED
	{syscall.Errno(10065), wasi.EHOSTUNREACH},  // WSAEHOSTUNREACH
	{syscall.Errno(64), wasi.ECONNRESET},       // ERROR_NETNAME_DELETED
	{syscall.Errno(1225), wasi.ECONNREFUSED},   // ERROR_CONNECTION_REFUSED
	{syscall.Errno(1231), wasi.ENETUNREACH},    // ERROR_NETWORK_UNREACHABLE
	{syscall.Errno(1232), wasi.EHOSTUNREACH},   // ERROR_HOST_UNREACHABLE
	{syscall.Errno(1236), wasi.ECONNABORTED},   // ERROR_CONNECTION_ABORTED
	{syscall.Errno(1234), wasi.ECONNREFUSED},   // ERROR_PORT_UNREACHABLE
}
