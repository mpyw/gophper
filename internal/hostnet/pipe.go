//declscope:namespace socket

package hostnet

import (
	"context"
	"errors"
	"net"
	"os"
)

// Pipes for PHP. gophper-wasm's compat/gophper_proc.c implements pipe(2)
// with pipe_open, and the guest then treats each end as a stream socket. An
// end is an os.File, so that a child process can be given it as is.

// socketPipeEnd tells which end of a pipe a socket is, if any.
type socketPipeEnd int8

const (
	socketPipeNone socketPipeEnd = iota
	socketPipeRead
	socketPipeWrite
)

// pipeOpen makes the guest fds readFD and writeFD the two ends of a new pipe.
func (x socketExports) pipeOpen(ctx context.Context, readFD, writeFD int32) int32 {
	t := x.from(ctx)
	r, w, err := os.Pipe()
	if err != nil {
		return errnoFrom(err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[readFD] = &socketEntry{kind: socketUnix, conn: socketPipeConn{r}, pipe: socketPipeRead, lazy: true, shutWrite: true}
	t.entries[writeFD] = &socketEntry{kind: socketUnix, conn: socketPipeConn{w}, pipe: socketPipeWrite, lazy: true}
	return 0
}

// pair makes the guest fds a and b a connected pair of Unix stream sockets.
func (x socketExports) pair(ctx context.Context, a, b int32) int32 {
	t := x.from(ctx)
	ca, cb, err := socketPair()
	if err != nil {
		return errnoFrom(err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[a] = &socketEntry{kind: socketUnix, conn: ca, lazy: true}
	t.entries[b] = &socketEntry{kind: socketUnix, conn: cb, lazy: true}
	return 0
}

// startReading starts reading a lazy end ahead. t.mu must be held.
func (t *Sockets) startReading(e *socketEntry) {
	if e.reading || !e.lazy || e.pipe == socketPipeWrite || e.closed {
		return
	}
	e.reading = true
	if e.pipe != socketPipeRead {
		go t.readStream(e, e.conn)
		return
	}
	done := make(chan struct{})
	e.readDone = done
	go func() {
		defer close(done)
		t.readStream(e, e.conn)
	}()
}

// ChildFile returns the host file for the guest fd, to give a child process,
// and a function to call once the child has started. Only pipes and stream
// sockets have one.
//
// A pipe end is returned as is: the child is started within the host call,
// and the guest closes its own end only afterwards.
func (t *Sockets) ChildFile(fd int32) (*os.File, func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil || e.closed {
		return nil, nil, os.ErrClosed
	}
	if e.lazy && e.reading {
		return nil, nil, errors.New("hostnet: the fd was read ahead, so a child cannot take it over")
	}
	switch c := e.conn.(type) {
	case socketPipeConn:
		return c.File, func() {}, nil
	case interface{ File() (*os.File, error) }:
		// *net.TCPConn and *net.UnixConn return a duplicate.
		f, err := c.File()
		if err != nil {
			return nil, nil, err
		}
		// The duplicate is released once the child has its own copy, so a
		// close error affects neither side.
		return f, func() { _ = f.Close() }, nil
	}
	return nil, nil, errors.New("hostnet: this socket cannot be given to a child process")
}

// socketPipeConn is a pipe end as a net.Conn.
type socketPipeConn struct{ *os.File }

func (socketPipeConn) LocalAddr() net.Addr  { return socketPipeAddr{} }
func (socketPipeConn) RemoteAddr() net.Addr { return socketPipeAddr{} }

type socketPipeAddr struct{}

// socketPipeReader closes a pipe's read end that is read ahead, once the
// read has ended. See socketPipeClose.
type socketPipeReader struct {
	file *os.File
	done <-chan struct{}
}

func (r socketPipeReader) Close() error { return socketPipeClose(r.file, r.done) }

func (socketPipeAddr) Network() string { return "pipe" }
func (socketPipeAddr) String() string  { return "" }
