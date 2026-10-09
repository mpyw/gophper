package hostproc

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"syscall"
	"testing"

	"github.com/mpyw/gophper/internal/wasi"
)

func TestErrnoFrom(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int32
	}{
		{nil, 0},
		{exec.ErrNotFound, wasi.ENOENT},
		{&fs.PathError{Op: "fork/exec", Path: "/x", Err: syscall.ENOENT}, wasi.ENOENT},
		{&fs.PathError{Op: "fork/exec", Path: "/x", Err: syscall.EACCES}, wasi.EACCES},
		{&fs.PathError{Op: "fork/exec", Path: "/x", Err: syscall.ENOEXEC}, wasi.ENOEXEC},
		{fmt.Errorf("signal: %w", syscall.ESRCH), wasi.ESRCH},
		{errors.New("something else"), wasi.EIO},
	} {
		if got := errnoFrom(tt.err); got != tt.want {
			t.Errorf("errnoFrom(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
