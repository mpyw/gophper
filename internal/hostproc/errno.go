package hostproc

import (
	"errors"
	"io/fs"
	"os/exec"
	"syscall"

	"github.com/mpyw/gophper/internal/wasi"
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
		return wasi.ENOENT
	case errors.Is(err, fs.ErrPermission):
		return wasi.EACCES
	case errors.Is(err, syscall.ENOEXEC):
		return wasi.ENOEXEC
	case errors.Is(err, syscall.ESRCH):
		return wasi.ESRCH
	}
	return wasi.EIO
}
