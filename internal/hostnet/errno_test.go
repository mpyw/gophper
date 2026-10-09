package hostnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestErrnoFrom(t *testing.T) {
	// Each host errno, as the net package wraps it.
	for _, m := range errnoBySyscall {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", m.sys)}
		if got := errnoFrom(err); got != m.wasi {
			t.Errorf("errnoFrom(%v) = %d, want %d", m.sys, got, m.wasi)
		}
	}
	for _, c := range []struct {
		err  error
		want int32
	}{
		{nil, 0},
		{&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, errnoETIMEDOUT},
		{fmt.Errorf("write: %w", net.ErrClosed), errnoEBADF},
		{&net.OpError{Op: "dial", Err: context.Canceled}, errnoEINTR},
		{&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}}, errnoEHOSTUNREACH},
		{errors.New("anything else"), errnoEIO},
		{syscall.EPERM, errnoEACCES},
	} {
		if got := errnoFrom(c.err); got != c.want {
			t.Errorf("errnoFrom(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
