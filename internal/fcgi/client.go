//declscope:namespace fcgi

package fcgi

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Do sends one request to a FastCGI responder over conn, and copies its
// FCGI_STDOUT and FCGI_STDERR to stdout and stderr as they arrive. It
// returns the application status from FCGI_END_REQUEST. conn carries this
// request only: no FCGI_KEEP_CONN.
//
// stdin is sent while the response is read, as a responder may read its
// body only after it starts writing. When ctx is done, conn is closed.
func Do(ctx context.Context, conn net.Conn, params map[string]string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// abort wakes the reader. The request may be in flight, so a failed
	// SetDeadline cannot be returned: close conn instead.
	abort := func() {
		if conn.SetDeadline(time.Unix(1, 0)) != nil {
			_ = conn.Close() // the reader reports the failure
		}
	}
	stop := context.AfterFunc(ctx, abort)
	defer stop()

	c := &serverConn{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
	const id = 1
	writeErr := make(chan error, 1)
	written := make(chan struct{})
	go func() {
		defer close(written)
		err := c.sendRequest(id, params, stdin)
		writeErr <- err
		if err != nil {
			// The responder may wait for the rest of the body forever. The
			// reader wakes and finds err.
			abort()
		}
	}()
	// The responder may answer before the writer read stdin to its end. The
	// caller may then read stdin again, as the HTTP router does, so the
	// writer stops before Do returns.
	defer func() {
		abort()
		<-written
	}()

	for {
		rec, err := c.readRecord()
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			select {
			case werr := <-writeErr:
				if werr != nil {
					return 0, fmt.Errorf("fcgi: sending the request: %w", werr)
				}
			default:
			}
			if errors.Is(err, io.EOF) {
				return 0, errors.New("fcgi: the responder closed the connection before FCGI_END_REQUEST")
			}
			return 0, err
		}
		if rec.h.ID != id {
			continue
		}
		switch rec.h.Type {
		case typeStdout:
			if _, err := stdout.Write(rec.content); err != nil {
				return 0, err
			}
		case typeStderr:
			if _, err := stderr.Write(rec.content); err != nil {
				return 0, err
			}
		case typeEndRequest:
			if len(rec.content) < 8 {
				return 0, errors.New("fcgi: short FCGI_END_REQUEST")
			}
			if s := rec.content[4]; s != protocolStatusRequestComplete {
				return 0, fmt.Errorf("fcgi: the responder refused the request (protocol status %d)", s)
			}
			return int(binary.BigEndian.Uint32(rec.content)), nil
		}
	}
}

// sendRequest writes FCGI_BEGIN_REQUEST, the params and the body.
func (c *serverConn) sendRequest(id uint16, params map[string]string, stdin io.Reader) error {
	begin := []byte{0, roleResponder, 0, 0, 0, 0, 0, 0}
	if err := c.writeRecord(typeBeginRequest, id, begin); err != nil {
		return err
	}
	pairs := encodePairs(params)
	for len(pairs) > 0 {
		n := min(len(pairs), maxContentLength)
		if err := c.writeRecord(typeParams, id, pairs[:n]); err != nil {
			return err
		}
		pairs = pairs[n:]
	}
	if err := c.writeRecord(typeParams, id, nil); err != nil {
		return err
	}
	if err := c.flush(); err != nil {
		return err
	}
	if stdin != nil {
		buf := make([]byte, maxContentLength)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if werr := c.writeRecord(typeStdin, id, buf[:n]); werr != nil {
					return werr
				}
				if werr := c.flush(); werr != nil {
					return werr
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
	}
	if err := c.writeRecord(typeStdin, id, nil); err != nil {
		return err
	}
	return c.flush()
}
