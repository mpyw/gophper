//go:build unix

//declscope:namespace pool

package server

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestPoolDialBeforeListen connects to a worker whose socket is bound but
// not listening yet, as between php-cgi's bind() and listen(). The refused
// connection is tried again.
func TestPoolDialBeforeListen(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "w.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		_ = syscall.Close(fd) // the bind error is the one to report
		t.Fatal(err)
	}
	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := syscall.Listen(fd, 1); err != nil {
			t.Error(err)
		}
		l, err := net.FileListener(os.NewFile(uintptr(fd), sock))
		if err != nil {
			t.Error(err)
		}
		listening <- l
	}()
	p := &pool{}
	w := &poolWorker{sock: sock, done: make(chan struct{}), listenBy: time.Now().Add(10 * time.Second)}
	conn, err := p.poolDial(w)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close() // only the connection mattered
	if l := <-listening; l != nil {
		_ = l.Close() // the test is over
	}
	if !w.listenBy.IsZero() {
		t.Error("listenBy is still set after the first connection")
	}
	// After that, a refused connection fails at once.
	w2 := &poolWorker{sock: filepath.Join(dir, "missing.sock"), done: make(chan struct{})}
	if _, err := p.poolDial(w2); err == nil {
		t.Error("connected to nothing")
	}
}
