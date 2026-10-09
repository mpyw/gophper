//declscope:namespace fcgi

package fcgi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fcgiDial connects to addr for Do, until the test ends.
func fcgiDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// fcgiFakeResponder accepts one connection and runs respond on it. An error
// from respond fails the test.
func fcgiFakeResponder(t *testing.T, respond func(c *serverConn) error) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		if err := respond(fcgiRawConnFrom(conn)); err != nil {
			t.Errorf("responder: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
	})
	return l.Addr().String()
}

func fcgiRawConnFrom(conn net.Conn) *serverConn {
	return &serverConn{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
}

// fcgiReadRequest reads a request up to the end of its body.
func (c *serverConn) fcgiReadRequest() {
	for {
		rec, err := c.readRecord()
		if err != nil || rec.h.Type == typeStdin && len(rec.content) == 0 {
			return
		}
	}
}

func TestDoRoundTrip(t *testing.T) {
	addr := fcgiTestServer(t, fcgiEcho)
	long := strings.Repeat("p", 100_000) // more than one FCGI_PARAMS record
	body := strings.Repeat("b", 3*maxContentLength+1)
	var stdout, stderr bytes.Buffer
	code, err := Do(context.Background(), fcgiDial(t, addr),
		map[string]string{"NAME": "do", "LONG": long, "STATUS": "5"},
		strings.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Content-Type: text/plain\r\n\r\ndo|100000|" + body; stdout.String() != want {
		t.Errorf("stdout: %d bytes, want %d", stdout.Len(), len(want))
	}
	if code != 5 || stderr.String() != "logged" {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestDoNoStdin(t *testing.T) {
	addr := fcgiTestServer(t, fcgiEcho)
	var stdout bytes.Buffer
	code, err := Do(context.Background(), fcgiDial(t, addr), map[string]string{"NAME": "empty"}, nil, &stdout, io.Discard)
	if err != nil || code != 0 || !strings.HasSuffix(stdout.String(), "empty|0|") {
		t.Errorf("code %d, err %v, stdout %q", code, err, stdout.String())
	}
}

func TestDoResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(c *serverConn) error
		code    int
		stdout  string
		stderr  string
		err     string
	}{
		{
			name: "records of other requests are ignored",
			respond: func(c *serverConn) error {
				c.fcgiReadRequest()
				return errors.Join(
					c.writeRecord(typeStdout, 2, []byte("not ours")),
					c.writeRecord(typeStdout, 1, []byte("ours")),
					c.writeRecord(typeStderr, 1, []byte("warn")),
					c.writeRecord(typeGetValuesResult, 1, nil),
					c.endRequest(1, 42, protocolStatusRequestComplete, nil),
				)
			},
			code: 42, stdout: "ours", stderr: "warn",
		},
		{
			name: "short FCGI_END_REQUEST",
			respond: func(c *serverConn) error {
				c.fcgiReadRequest()
				return errors.Join(c.writeRecord(typeEndRequest, 1, []byte{0, 0, 0}), c.flush())
			},
			err: "short FCGI_END_REQUEST",
		},
		{
			name: "refused",
			respond: func(c *serverConn) error {
				c.fcgiReadRequest()
				return c.endRequest(1, 0, protocolStatusCantMpxConn, nil)
			},
			err: "refused the request (protocol status 1)",
		},
		{
			name: "closed before the end",
			respond: func(c *serverConn) error {
				c.fcgiReadRequest()
				return errors.Join(c.writeRecord(typeStdout, 1, []byte("partial")), c.flush())
			},
			stdout: "partial",
			err:    "closed the connection before FCGI_END_REQUEST",
		},
		{
			name: "bad record",
			respond: func(c *serverConn) error {
				c.fcgiReadRequest()
				_, err := c.conn.Write([]byte{9, typeStdout, 0, 1, 0, 0, 0, 0})
				return err
			},
			err: "unsupported version 9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := fcgiFakeResponder(t, tc.respond)
			var stdout, stderr bytes.Buffer
			code, err := Do(context.Background(), fcgiDial(t, addr), map[string]string{"A": "1"}, nil, &stdout, &stderr)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Errorf("err = %v, want %q", err, tc.err)
			}
			if code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
				t.Errorf("code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDoCanceled(t *testing.T) {
	addr := fcgiFakeResponder(t, func(c *serverConn) error {
		c.fcgiReadRequest()
		// Never answers, until the client gives up and closes the
		// connection, which ends this read with an error.
		_, _ = c.readRecord()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Do(ctx, fcgiDial(t, addr), nil, nil, io.Discard, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

type fcgiFailingWriter struct{ err error }

func (w fcgiFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDoWriterFails(t *testing.T) {
	for _, stream := range []uint8{typeStdout, typeStderr} {
		t.Run(strconv.Itoa(int(stream)), func(t *testing.T) {
			addr := fcgiFakeResponder(t, func(c *serverConn) error {
				c.fcgiReadRequest()
				return errors.Join(
					c.writeRecord(stream, 1, []byte("x")),
					c.endRequest(1, 0, protocolStatusRequestComplete, nil),
				)
			})
			full := errors.New("disk full")
			stdout, stderr := io.Writer(io.Discard), io.Writer(io.Discard)
			if stream == typeStdout {
				stdout = fcgiFailingWriter{full}
			} else {
				stderr = fcgiFailingWriter{full}
			}
			if _, err := Do(context.Background(), fcgiDial(t, addr), nil, nil, stdout, stderr); !errors.Is(err, full) {
				t.Errorf("err = %v, want %v", err, full)
			}
		})
	}
}

// fcgiFailingReader returns data, then an error.
type fcgiFailingReader struct {
	data []byte
	err  error
}

func (r *fcgiFailingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// A body that fails to read ends the request, although the responder
// still waits for the rest of it.
func TestDoStdinFails(t *testing.T) {
	addr := fcgiFakeResponder(t, func(c *serverConn) error {
		// Reads until the client closes the connection, which is the end
		// this scenario expects, so the read error is not a failure.
		for {
			if _, err := c.readRecord(); err != nil {
				return nil
			}
		}
	})
	broken := errors.New("broken body")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Do(ctx, fcgiDial(t, addr), nil, &fcgiFailingReader{data: []byte("start"), err: broken}, io.Discard, io.Discard)
	if !errors.Is(err, broken) || !strings.Contains(err.Error(), "sending the request") {
		t.Errorf("err = %v, want %v", err, broken)
	}
}
