//declscope:namespace fcgi

package fcgi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests here drive serverConn over net.Pipe, whose writes wait for the
// reader. That lets a test hold a write, or the lock behind it, at the
// point it needs, where TCP would buffer it.

// fcgiFakeConn wraps a connection to make its writes or deadlines fail.
type fcgiFakeConn struct {
	net.Conn

	mu sync.Mutex
	// writes is how many Writes succeed before every one fails with
	// writeErr. With writeErr nil, all succeed.
	writes   int
	writeErr error
	// deadlineErr, unless nil, fails SetDeadline.
	deadlineErr error
	// readDeadline, unless nil, may fail SetReadDeadline before the
	// connection sees it.
	readDeadline func(time.Time) error
}

func (c *fcgiFakeConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	var err error
	if c.writeErr != nil {
		if c.writes == 0 {
			err = c.writeErr
		}
		c.writes--
	}
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c *fcgiFakeConn) SetDeadline(t time.Time) error {
	if c.deadlineErr != nil {
		return c.deadlineErr
	}
	return c.Conn.SetDeadline(t)
}

func (c *fcgiFakeConn) SetReadDeadline(t time.Time) error {
	if c.readDeadline != nil {
		if err := c.readDeadline(t); err != nil {
			return err
		}
	}
	return c.Conn.SetReadDeadline(t)
}

// fcgiHalfConn adds CloseWrite, which net.Pipe lacks, so that lingerClose
// takes the path it takes on TCP.
type fcgiHalfConn struct {
	*fcgiFakeConn
	closeWriteErr error
}

// CloseWrite does nothing: the other end sees no EOF, but nothing here
// reads after the response.
func (c fcgiHalfConn) CloseWrite() error { return c.closeWriteErr }

// fcgiPipe returns both ends of a net.Pipe, closed when the test ends.
// Reads from client time out, so that a test that fails does not hang.
func fcgiPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	client, server = net.Pipe()
	t.Cleanup(func() {
		// A second Close of a pipe end returns nil; the first already ended it.
		_ = client.Close()
		_ = server.Close()
	})
	// Set now: a pipe whose other end closed takes no deadline.
	if err := client.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return client, server
}

// fcgiServeConn runs serve on conn with h. The returned channel is closed
// when serve returns.
func fcgiServeConn(t *testing.T, conn net.Conn, h Handler) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fcgiRawConnFrom(conn).serve(context.Background(), h)
	}()
	t.Cleanup(func() {
		// Wakes serve if the test left it reading; serve closes conn again.
		_ = conn.Close()
		<-done
	})
	return done
}

// fcgiWaitDone fails the test unless done closes soon.
func fcgiWaitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not end", what)
	}
}

// fcgiExpectEOF checks that the other end of conn, from fcgiPipe, closed it.
func fcgiExpectEOF(t *testing.T, conn net.Conn) {
	t.Helper()
	if n, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("Read = %d, %v, want io.EOF", n, err)
	}
}

// A failed write means the web server is gone, and serve ends.
func TestServeConnWriteFails(t *testing.T) {
	for name, send := range map[string]func(t *testing.T, c *serverConn){
		"FCGI_GET_VALUES_RESULT": func(t *testing.T, c *serverConn) {
			c.fcgiSend(t, typeGetValues, 0, nil)
		},
		"FCGI_UNKNOWN_TYPE": func(t *testing.T, c *serverConn) {
			c.fcgiSend(t, 99, 0, nil)
		},
		"unknown role": func(t *testing.T, c *serverConn) {
			c.fcgiBegin(t, 1, 2, false)
		},
		"a second request": func(t *testing.T, c *serverConn) {
			c.fcgiBegin(t, 1, roleResponder, true)
			c.fcgiBegin(t, 2, roleResponder, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			client, server := fcgiPipe(t)
			done := fcgiServeConn(t, server, fcgiEcho)
			// serve has read the record once the send returns. Its reply
			// then finds the pipe closed.
			send(t, fcgiRawConnFrom(client))
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			fcgiWaitDone(t, done, "serve")
		})
	}
}

// A connection whose read deadline cannot be set is closed: it could
// otherwise hang.
func TestServeConnDeadlineFails(t *testing.T) {
	broken := errors.New("broken deadline")
	for _, tc := range []struct {
		name string
		// fail decides which SetReadDeadline fails. nonZero counts the
		// calls with a deadline, including this one.
		fail func(t time.Time, nonZero int) bool
		send func(t *testing.T, c *serverConn)
		// handled is whether the handler runs.
		handled bool
	}{
		{
			name: "idle",
			fail: func(time.Time, int) bool { return true },
			send: func(*testing.T, *serverConn) {},
		},
		{
			name: "params in",
			fail: func(t time.Time, _ int) bool { return t.IsZero() },
			send: func(t *testing.T, c *serverConn) {
				c.fcgiBegin(t, 1, roleResponder, true)
				c.fcgiSend(t, typeParams, 1, nil)
			},
		},
		{
			// The first is serve's idle deadline, and the second handle's,
			// before FCGI_END_REQUEST.
			name: "kept alive",
			fail: func(t time.Time, nonZero int) bool { return nonZero == 2 },
			send: func(t *testing.T, c *serverConn) {
				c.fcgiBegin(t, 1, roleResponder, true)
				c.fcgiSend(t, typeParams, 1, nil)
			},
			handled: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := fcgiPipe(t)
			var mu sync.Mutex
			var nonZero int
			conn := &fcgiFakeConn{Conn: server, readDeadline: func(d time.Time) error {
				mu.Lock()
				defer mu.Unlock()
				if !d.IsZero() {
					nonZero++
				}
				if tc.fail(d, nonZero) {
					return broken
				}
				return nil
			}}
			var handled atomic.Bool
			done := fcgiServeConn(t, conn, func(context.Context, *Request) int {
				handled.Store(true)
				return 0
			})
			tc.send(t, fcgiRawConnFrom(client))
			// No FCGI_END_REQUEST: the connection just closes.
			fcgiExpectEOF(t, client)
			fcgiWaitDone(t, done, "serve")
			if handled.Load() != tc.handled {
				t.Errorf("handled = %t, want %t", handled.Load(), tc.handled)
			}
		})
	}
}

// fcgiHeldResponse starts a request without FCGI_KEEP_CONN on a pipe, and
// returns once handle holds mu, blocked writing FCGI_END_REQUEST. client
// reads the rest of the response, and writes to serve.
func fcgiHeldResponse(t *testing.T) (client *serverConn, done <-chan struct{}) {
	t.Helper()
	end, server := fcgiPipe(t)
	done = fcgiServeConn(t, fcgiHalfConn{fcgiFakeConn: &fcgiFakeConn{Conn: server}}, func(context.Context, *Request) int { return 0 })
	c := fcgiRawConnFrom(end)
	c.fcgiBegin(t, 1, roleResponder, false)
	// A body before the params is dropped: no handler reads it yet.
	c.fcgiSend(t, typeStdin, 1, []byte("early"))
	c.fcgiSend(t, typeParams, 1, nil)
	// The only write so far is endRequest's flush, which holds mu, and
	// waits until all of it is read.
	first := make([]byte, 1)
	if _, err := io.ReadFull(end, first); err != nil {
		t.Fatal(err)
	}
	c.r = bufio.NewReader(io.MultiReader(bytes.NewReader(first), end))
	return c, done
}

// fcgiWaitForServe waits until serve's goroutine is in fn, such as
// blocked on a lock there. Nothing else shows it.
func fcgiWaitForServe(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for g := range strings.SplitSeq(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(g, "(*serverConn).serve(") && strings.Contains(g, "(*serverConn)."+fn+"(") {
				return
			}
		}
	}
	t.Fatalf("serve never reached %s", fn)
}

// The next request may arrive before handle closes done. serve waits for
// mu, and sees the request finished.
func TestServeConnNextRequestWaits(t *testing.T) {
	c, done := fcgiHeldResponse(t)
	c.fcgiBegin(t, 2, roleResponder, false)
	if res := c.fcgiReadResult(t, 1); res.protocolStatus != protocolStatusRequestComplete {
		t.Fatalf("protocol status %d", res.protocolStatus)
	}
	// Without FCGI_KEEP_CONN, serve only drains what comes after.
	c.fcgiSend(t, typeStdin, 2, []byte("dropped"))
	if err := c.conn.Close(); err != nil {
		t.Fatal(err)
	}
	fcgiWaitDone(t, done, "serve")
}

// A management record during the response is answered after it. Then the
// request is finished before serve reads again.
func TestServeConnFinishedWhileAnswering(t *testing.T) {
	c, done := fcgiHeldResponse(t)
	c.fcgiSend(t, typeGetValues, 0, nil)
	// serve has seen the request unfinished, and waits for mu.
	fcgiWaitForServe(t, "writeRecord")
	if res := c.fcgiReadResult(t, 1); res.protocolStatus != protocolStatusRequestComplete {
		t.Fatalf("protocol status %d", res.protocolStatus)
	}
	rec, err := c.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	if rec.h.Type != typeGetValuesResult {
		t.Errorf("record type %d, want FCGI_GET_VALUES_RESULT", rec.h.Type)
	}
	if err := c.conn.Close(); err != nil {
		t.Fatal(err)
	}
	fcgiWaitDone(t, done, "serve")
}

// Once stopped, the reader's deadline stays put, and failing to set it
// closes the connection instead.
func TestServeConnStop(t *testing.T) {
	client, server := fcgiPipe(t)
	c := fcgiRawConnFrom(&fcgiFakeConn{Conn: server, readDeadline: func(time.Time) error { return errors.New("broken deadline") }})
	c.stop()
	fcgiExpectEOF(t, client)
	if !c.readDeadline(time.Now()) {
		t.Error("readDeadline after stop failed")
	}
}

// lingerClose closes at once when it cannot half-close and wait.
func TestServeConnLingerClose(t *testing.T) {
	broken := errors.New("broken")
	for name, wrap := range map[string]func(net.Conn) net.Conn{
		"no CloseWrite": func(c net.Conn) net.Conn { return c },
		"CloseWrite fails": func(c net.Conn) net.Conn {
			return fcgiHalfConn{fcgiFakeConn: &fcgiFakeConn{Conn: c}, closeWriteErr: broken}
		},
		"deadline fails": func(c net.Conn) net.Conn {
			return fcgiHalfConn{fcgiFakeConn: &fcgiFakeConn{Conn: c, readDeadline: func(time.Time) error { return broken }}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			client, server := fcgiPipe(t)
			fcgiRawConnFrom(wrap(server)).lingerClose()
			fcgiExpectEOF(t, client)
		})
	}
}

// A write that failed before leaves the buffer failing, and endRequest
// still closes done.
func TestServeConnEndRequestFails(t *testing.T) {
	_, server := fcgiPipe(t)
	broken := errors.New("broken")
	c := fcgiRawConnFrom(&fcgiFakeConn{Conn: server, writeErr: broken})
	if _, err := c.w.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	if err := c.w.Flush(); !errors.Is(err, broken) {
		t.Fatalf("Flush = %v, want %v", err, broken)
	}
	done := make(chan struct{})
	if err := c.endRequest(1, 0, protocolStatusRequestComplete, done); !errors.Is(err, broken) {
		t.Errorf("endRequest = %v, want %v", err, broken)
	}
	select {
	case <-done:
	default:
		t.Error("done is still open")
	}
}
