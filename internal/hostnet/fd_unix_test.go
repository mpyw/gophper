//go:build !windows

//declscope:namespace socket

package hostnet

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// socketExhaustFDs leaves only free fds open to this process, under a lower
// limit so that few are needed. It returns a func that gives them back,
// to be called before anything else can need one, t.Error included.
func socketExhaustFDs(t *testing.T, free int) func() {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	low := old
	low.Cur = min(old.Cur, 256)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	var fds []int
	release := func() {
		for _, fd := range fds {
			_ = syscall.Close(fd) // our own /dev/null fds
		}
		fds = nil
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) // back to what it was
	}
	for {
		fd, err := syscall.Open("/dev/null", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if errors.Is(err, syscall.EMFILE) {
			break
		}
		if err != nil {
			release()
			t.Fatal(err)
		}
		fds = append(fds, fd)
	}
	if len(fds) < free {
		release()
		t.Fatalf("only %d fds to free", len(fds))
	}
	for _, fd := range fds[len(fds)-free:] {
		_ = syscall.Close(fd) // our own /dev/null fds
	}
	fds = fds[:len(fds)-free]
	return release
}

// TestSocketNoFDs fails each call that needs a new host fd when none is
// left, with an errno for PHP.
func TestSocketNoFDs(t *testing.T) {
	h := newSocketHarness(t)
	h.open(10, socketUDP)
	for _, tt := range []struct {
		name string
		free int
		do   func() int32
	}{
		{"pipe_open", 0, func() int32 { return h.x.pipeOpen(h.ctx, 1, 2) }},
		{"sock_pair", 0, func() int32 { return h.x.pair(h.ctx, 1, 2) }},
		// socketpair(2) takes both, and then no fd is left for the dup.
		{"sock_pair's dup", 2, func() int32 { return h.x.pair(h.ctx, 1, 2) }},
		// sendto(2) on an unbound socket binds it first.
		{"sendto unbound", 0, func() int32 { return -h.send(10, "x", "127.0.0.1:9", 0) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			release := socketExhaustFDs(t, tt.free)
			got := tt.do()
			release()
			if got == 0 {
				t.Errorf("succeeded with %d fds free", tt.free)
			}
		})
	}
}

// TestSocketPairSecondEndFails closes the first end when the second one
// cannot be made, and reports why.
func TestSocketPairSecondEndFails(t *testing.T) {
	fileConn := socketFileConn
	t.Cleanup(func() { socketFileConn = fileConn })
	var first net.Conn
	socketFileConn = func(f *os.File) (net.Conn, error) {
		if first != nil {
			return nil, errSocketBroken
		}
		c, err := fileConn(f)
		first = c
		return c, err
	}
	if _, _, err := socketPair(); !errors.Is(err, errSocketBroken) {
		t.Errorf("err = %v", err)
	}
	if first == nil {
		t.Fatal("no first end")
	}
	if _, err := first.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("first end still open: %v", err)
	}
}
