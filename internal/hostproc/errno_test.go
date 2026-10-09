package hostproc

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"syscall"
	"testing"
)

func TestErrnoFrom(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int32
	}{
		{nil, 0},
		{exec.ErrNotFound, errnoENOENT},
		{&fs.PathError{Op: "fork/exec", Path: "/x", Err: syscall.ENOENT}, errnoENOENT},
		{&fs.PathError{Op: "fork/exec", Path: "/x", Err: syscall.EACCES}, errnoEACCES},
		{&fs.PathError{Op: "fork/exec", Path: "/x", Err: syscall.ENOEXEC}, errnoENOEXEC},
		{fmt.Errorf("signal: %w", syscall.ESRCH), errnoESRCH},
		{errors.New("something else"), errnoEIO},
	} {
		if got := errnoFrom(tt.err); got != tt.want {
			t.Errorf("errnoFrom(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
