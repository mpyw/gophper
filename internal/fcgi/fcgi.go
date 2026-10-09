// Package fcgi implements the responder role of the FastCGI protocol.
//
// net/http/fcgi is not used because it drops SCRIPT_NAME and PATH_INFO,
// which PHP needs verbatim in $_SERVER. Here every FCGI_PARAMS pair reaches
// the handler unchanged, and the handler writes the raw CGI response
// (headers included) to stdout, as php-fpm does.
//
// Requests on one connection are served one at a time. Multiplexing is
// refused through FCGI_MPXS_CONNS=0, which is what nginx and Apache expect.
package fcgi

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Record types.
const (
	typeBeginRequest    uint8 = 1
	typeAbortRequest    uint8 = 2
	typeEndRequest      uint8 = 3
	typeParams          uint8 = 4
	typeStdin           uint8 = 5
	typeStdout          uint8 = 6
	typeStderr          uint8 = 7
	typeData            uint8 = 8
	typeGetValues       uint8 = 9
	typeGetValuesResult uint8 = 10
	typeUnknownType     uint8 = 11
)

// Roles and protocol statuses.
const (
	roleResponder = 1

	protocolStatusRequestComplete = 0
	protocolStatusCantMpxConn     = 1
	protocolStatusUnknownRole     = 3
)

const (
	flagKeepConn = 1
	// maxContentLength is the largest content one record can carry.
	maxContentLength = 65535
)

// Request is one FastCGI request.
type Request struct {
	// Params holds every FCGI_PARAMS pair, unchanged.
	Params map[string]string
	// Stdin streams the FCGI_STDIN body.
	Stdin io.Reader
	// Stdout and Stderr become FCGI_STDOUT and FCGI_STDERR records.
	Stdout io.Writer
	Stderr io.Writer
}

// Handler serves one request and returns the application exit status.
// ctx is canceled when the web server aborts the request or the server stops.
type Handler func(ctx context.Context, r *Request) int

// Serve accepts connections on l until ctx is done or l fails.
func Serve(ctx context.Context, l net.Listener, h Handler) error {
	go func() {
		<-ctx.Done()
		// Only wakes Accept. Its error is the one Serve sees.
		_ = l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Go(func() {
			c := &serverConn{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
			c.serve(ctx, h)
		})
	}
}

type header struct {
	Version       uint8
	Type          uint8
	ID            uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

type record struct {
	h       header
	content []byte
}

// serverConn is one web server connection. Writes are serialized by mu because
// stdout and stderr are written from the handler while the reader keeps
// consuming FCGI_STDIN.
type serverConn struct {
	conn net.Conn
	r    *bufio.Reader

	// dmu guards read deadlines: once stopped, none moves later.
	dmu     sync.Mutex
	stopped bool

	mu sync.Mutex
	w  *bufio.Writer
}

func (c *serverConn) readRecord() (*record, error) {
	var rec record
	if err := binary.Read(c.r, binary.BigEndian, &rec.h); err != nil {
		return nil, err
	}
	if rec.h.Version != 1 {
		return nil, fmt.Errorf("fcgi: unsupported version %d", rec.h.Version)
	}
	buf := make([]byte, int(rec.h.ContentLength)+int(rec.h.PaddingLength))
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	rec.content = buf[:rec.h.ContentLength]
	return &rec, nil
}

func (c *serverConn) writeRecord(typ uint8, id uint16, content []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeRecordLocked(typ, id, content)
}

func (c *serverConn) writeRecordLocked(typ uint8, id uint16, content []byte) error {
	pad := uint8(-len(content) & 7)
	h := header{Version: 1, Type: typ, ID: id, ContentLength: uint16(len(content)), PaddingLength: pad}
	if err := binary.Write(c.w, binary.BigEndian, h); err != nil {
		return err
	}
	if _, err := c.w.Write(content); err != nil {
		return err
	}
	var zeros [8]byte
	_, err := c.w.Write(zeros[:pad])
	return err
}

func (c *serverConn) flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Flush()
}

// endRequest writes FCGI_END_REQUEST. done, unless nil, is closed before
// another write can start. See serve.
func (c *serverConn) endRequest(id uint16, appStatus uint32, protocolStatus uint8, done chan struct{}) error {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, appStatus)
	b[4] = protocolStatus
	c.mu.Lock()
	defer c.mu.Unlock()
	if done != nil {
		defer close(done)
	}
	if err := c.writeRecordLocked(typeEndRequest, id, b); err != nil {
		return err
	}
	return c.w.Flush()
}

// activeRequest is the request in progress on a connection.
type activeRequest struct {
	id       uint16
	keepConn bool
	params   []byte
	// started is set once the empty FCGI_PARAMS record arrives and the handler starts.
	started bool
	stdin   *io.PipeWriter
	cancel  context.CancelFunc
	done    chan struct{}
}

func (c *serverConn) serve(ctx context.Context, h Handler) {
	// handle may have closed conn already.
	defer func() { _ = c.conn.Close() }()
	// When the server stops, a reader blocked on an idle connection, such
	// as one nginx keeps alive, wakes and ends. Handlers see ctx too.
	defer context.AfterFunc(ctx, c.stop)()

	var cur *activeRequest
	defer func() {
		// A request whose params are still arriving has no handler yet.
		if cur != nil && cur.started {
			cur.cancel()
			_ = cur.stdin.CloseWithError(io.ErrUnexpectedEOF) // always nil
			<-cur.done
		}
	}()

	for {
		if cur != nil && cur.started {
			select {
			case <-cur.done:
				keep := cur.keepConn
				cur = nil
				if !keep {
					c.drain()
					return
				}
			default:
			}
		}

		if cur == nil && !c.readDeadline(time.Now().Add(serverIdle)) {
			return
		}
		rec, err := c.readRecord()
		if err != nil {
			return
		}

		// A finished request may complete while we were blocked in readRecord.
		if cur != nil && cur.started {
			if rec.h.Type == typeBeginRequest {
				// The web server sends the next request once it has read
				// FCGI_END_REQUEST, which may be before handle closed done.
				// handle holds mu until then.
				c.mu.Lock()
				c.mu.Unlock() //nolint:staticcheck // A barrier: it waits for handle to release mu.
			}
			select {
			case <-cur.done:
				keep := cur.keepConn
				cur = nil
				if !keep {
					c.drain()
					return
				}
			default:
			}
		}

		// A failed write means the web server is gone: returning closes conn.
		switch rec.h.Type {
		case typeGetValues:
			if c.writeRecord(typeGetValuesResult, 0, encodePairs(map[string]string{
				"FCGI_MPXS_CONNS": "0",
			})) != nil || c.flush() != nil {
				return
			}
			continue
		case typeBeginRequest:
			if len(rec.content) < 3 {
				return
			}
			role := binary.BigEndian.Uint16(rec.content)
			if cur != nil {
				if c.endRequest(rec.h.ID, 0, protocolStatusCantMpxConn, nil) != nil {
					return
				}
				continue
			}
			if role != roleResponder {
				if c.endRequest(rec.h.ID, 0, protocolStatusUnknownRole, nil) != nil {
					return
				}
				continue
			}
			cur = &activeRequest{id: rec.h.ID, keepConn: rec.content[2]&flagKeepConn != 0, done: make(chan struct{})}
			// A request may take its time, and the reader waits for its
			// records, or an FCGI_ABORT_REQUEST, all along.
			if !c.readDeadline(time.Time{}) {
				return
			}
			continue
		}

		if rec.h.ID == 0 {
			// Unknown management record.
			if c.writeRecord(typeUnknownType, 0, []byte{rec.h.Type, 0, 0, 0, 0, 0, 0, 0}) != nil || c.flush() != nil {
				return
			}
			continue
		}
		if cur == nil || rec.h.ID != cur.id {
			continue
		}

		switch rec.h.Type {
		case typeParams:
			if len(rec.content) > 0 {
				cur.params = append(cur.params, rec.content...)
				continue
			}
			params, err := decodePairs(cur.params)
			if err != nil {
				return
			}
			pr, pw := io.Pipe()
			reqCtx, cancel := context.WithCancel(ctx)
			cur.stdin, cur.cancel, cur.started = pw, cancel, true
			go c.handle(reqCtx, h, cur, &Request{
				Params: params,
				Stdin:  pr,
				Stdout: &streamWriter{c: c, typ: typeStdout, id: cur.id},
				Stderr: &streamWriter{c: c, typ: typeStderr, id: cur.id},
			}, pr)
		case typeStdin:
			if cur.stdin == nil {
				continue
			}
			if len(rec.content) == 0 {
				_ = cur.stdin.Close() // always nil
				continue
			}
			// Fails once the handler stopped reading. The body is then discarded.
			_, _ = cur.stdin.Write(rec.content)
		case typeAbortRequest:
			if cur.cancel != nil {
				cur.cancel()
			}
		case typeData:
			// Only used by the filter role.
		}
	}
}

func (c *serverConn) handle(ctx context.Context, h Handler, a *activeRequest, req *Request, stdin *io.PipeReader) {
	status := h(ctx, req)
	a.cancel()
	// Unblock the reader if the script did not consume the whole body.
	_ = stdin.CloseWithError(errors.New("fcgi: request finished")) // always nil

	err := c.writeRecord(typeStdout, a.id, nil)
	if err == nil {
		err = c.writeRecord(typeStderr, a.id, nil)
	}
	if err == nil {
		err = c.endRequest(a.id, uint32(status), protocolStatusRequestComplete, a.done)
	} else {
		// No FCGI_END_REQUEST was sent, so no next request can race the reader.
		close(a.done)
	}
	switch {
	case err != nil:
		// Wake the reader loop, which may be blocked waiting for a record.
		// serve closes conn again; the second error means nothing.
		_ = c.conn.Close()
	case !a.keepConn:
		c.lingerClose()
	}
}

// serverIdle is how long a connection may wait between requests.
const serverIdle = 2 * time.Minute

// readDeadline sets the read deadline, unless the connection is stopping.
// It reports false if setting it failed, and the connection is closed.
func (c *serverConn) readDeadline(t time.Time) bool {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if c.stopped {
		return true
	}
	if err := c.conn.SetReadDeadline(t); err != nil {
		_ = c.conn.Close() // A connection with no deadline could hang; its own error says why.
		return false
	}
	return true
}

// stop wakes the reader for good.
func (c *serverConn) stop() {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	c.stopped = true
	if c.conn.SetReadDeadline(time.Unix(1, 0)) != nil {
		_ = c.conn.Close() // Closing wakes it too.
	}
}

// serverLinger is how long a connection without keep-alive waits for the
// web server to close it, after the response.
const serverLinger = 5 * time.Second

// lingerClose ends the response with a FIN, and leaves the rest to the
// reader loop, which drains the connection until the web server closes it.
// Closing at once, with records still unread, such as FCGI_STDIN's last,
// would send a RST, and the web server could lose the end of the response.
func (c *serverConn) lingerClose() {
	cw, ok := c.conn.(interface{ CloseWrite() error })
	if !ok || cw.CloseWrite() != nil || !c.readDeadline(time.Now().Add(serverLinger)) {
		// No half-close here: closing is all that is left.
		_ = c.conn.Close()
	}
}

// drain reads and drops what the web server still sends, until it closes
// the connection or lingerClose's deadline passes.
func (c *serverConn) drain() {
	_, _ = io.Copy(io.Discard, c.r) // ends with the connection; its error is the expected one
}

// streamWriter frames writes into FCGI_STDOUT or FCGI_STDERR records.
type streamWriter struct {
	c   *serverConn
	typ uint8
	id  uint16
}

func (w *streamWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxContentLength {
			chunk = chunk[:maxContentLength]
		}
		if err := w.c.writeRecord(w.typ, w.id, chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, w.c.flush()
}

func decodePairs(b []byte) (map[string]string, error) {
	m := make(map[string]string)
	for len(b) > 0 {
		nameLen, n := readPairLength(b)
		if n == 0 {
			return nil, errors.New("fcgi: bad name length")
		}
		b = b[n:]
		valueLen, n := readPairLength(b)
		if n == 0 {
			return nil, errors.New("fcgi: bad value length")
		}
		b = b[n:]
		if uint32(len(b)) < nameLen+valueLen {
			return nil, errors.New("fcgi: truncated pair")
		}
		m[string(b[:nameLen])] = string(b[nameLen : nameLen+valueLen])
		b = b[nameLen+valueLen:]
	}
	return m, nil
}

func readPairLength(b []byte) (uint32, int) {
	if len(b) == 0 {
		return 0, 0
	}
	if b[0]>>7 == 0 {
		return uint32(b[0]), 1
	}
	if len(b) < 4 {
		return 0, 0
	}
	return binary.BigEndian.Uint32(b) &^ (1 << 31), 4
}

func encodePairs(m map[string]string) []byte {
	var b []byte
	for k, v := range m {
		b = appendPairLength(b, len(k))
		b = appendPairLength(b, len(v))
		b = append(b, k...)
		b = append(b, v...)
	}
	return b
}

func appendPairLength(b []byte, n int) []byte {
	if n < 128 {
		return append(b, byte(n))
	}
	return binary.BigEndian.AppendUint32(b, uint32(n)|1<<31)
}
