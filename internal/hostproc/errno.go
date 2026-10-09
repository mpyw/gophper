package hostproc

import (
	"errors"
	"io/fs"
	"os/exec"
	"syscall"
)

// WASI errno values, as wasi-libc numbers them (__errno_values.h).
//
//declscope:shared // process.go returns them from host functions
const (
	errnoEACCES  int32 = 2
	errnoEBADF   int32 = 8
	errnoECHILD  int32 = 12
	errnoEINTR   int32 = 27
	errnoEINVAL  int32 = 28
	errnoEIO     int32 = 29
	errnoENOENT  int32 = 44
	errnoENOEXEC int32 = 45
	errnoESRCH   int32 = 71
)

// errnoFrom maps an error from starting or signaling a process to the WASI
// errno posix_spawn or kill would return.
//
//declscope:shared // process.go
func errnoFrom(err error) int32 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return errnoENOENT
	case errors.Is(err, fs.ErrPermission):
		return errnoEACCES
	case errors.Is(err, syscall.ENOEXEC):
		return errnoENOEXEC
	case errors.Is(err, syscall.ESRCH):
		return errnoESRCH
	}
	return errnoEIO
}
