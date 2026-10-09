package hostnet

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
)

// WASI errno values, as wasi-libc numbers them (__errno_values.h).
// Host functions return these, so the guest sees them in errno.
//
//declscope:shared // socket.go returns them from host functions
const (
	errnoEAGAIN          int32 = 6
	errnoEALREADY        int32 = 7
	errnoEBADF           int32 = 8
	errnoEDESTADDRREQ    int32 = 17
	errnoEINPROGRESS     int32 = 26
	errnoEINTR           int32 = 27
	errnoEINVAL          int32 = 28
	errnoEIO             int32 = 29
	errnoEISCONN         int32 = 30
	errnoENOTCONN        int32 = 53
	errnoENOTSUP         int32 = 58
	errnoEPIPE           int32 = 64
	errnoEPROTONOSUPPORT int32 = 66
)

// More WASI errno values, only produced by errnoFrom.
const (
	errnoEACCES        int32 = 2
	errnoEADDRINUSE    int32 = 3
	errnoEADDRNOTAVAIL int32 = 4
	errnoEAFNOSUPPORT  int32 = 5
	errnoECONNABORTED  int32 = 13
	errnoECONNREFUSED  int32 = 14
	errnoECONNRESET    int32 = 15
	errnoEHOSTUNREACH  int32 = 23
	errnoEMSGSIZE      int32 = 35
	errnoENETUNREACH   int32 = 40
	errnoENOENT        int32 = 44
	errnoETIMEDOUT     int32 = 73
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
		return errnoETIMEDOUT
	case errors.Is(err, net.ErrClosed):
		return errnoEBADF
	case errors.Is(err, context.Canceled):
		return errnoEINTR
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errnoEHOSTUNREACH
	}
	return errnoEIO
}

var errnoBySyscall = []struct {
	sys  syscall.Errno
	wasi int32
}{
	{syscall.ECONNREFUSED, errnoECONNREFUSED},
	{syscall.ECONNRESET, errnoECONNRESET},
	{syscall.ECONNABORTED, errnoECONNABORTED},
	{syscall.ETIMEDOUT, errnoETIMEDOUT},
	{syscall.EHOSTUNREACH, errnoEHOSTUNREACH},
	{syscall.ENETUNREACH, errnoENETUNREACH},
	{syscall.EADDRINUSE, errnoEADDRINUSE},
	{syscall.EADDRNOTAVAIL, errnoEADDRNOTAVAIL},
	{syscall.EACCES, errnoEACCES},
	{syscall.EPERM, errnoEACCES},
	{syscall.EPIPE, errnoEPIPE},
	{syscall.ENOENT, errnoENOENT},
	{syscall.EAFNOSUPPORT, errnoEAFNOSUPPORT},
	{syscall.EINVAL, errnoEINVAL},
	{syscall.EMSGSIZE, errnoEMSGSIZE},
	{syscall.ENOTCONN, errnoENOTCONN},
	{syscall.EISCONN, errnoEISCONN},
}
