package fcgi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPairLengthEncoding(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []byte
	}{
		{0, []byte{0}},
		{127, []byte{127}},
		{128, []byte{0x80, 0, 0, 128}},
		{300, []byte{0x80, 0, 0x01, 0x2c}},
		{70000, []byte{0x80, 0x01, 0x11, 0x70}},
	} {
		got := appendPairLength(nil, tc.n)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("appendPairLength(%d) = %x, want %x", tc.n, got, tc.want)
		}
		n, size := readPairLength(got)
		if int(n) != tc.n || size != len(tc.want) {
			t.Errorf("readPairLength(%x) = %d, %d", got, n, size)
		}
	}
}

func TestPairsRoundTrip(t *testing.T) {
	m := map[string]string{
		"EMPTY":                  "",
		"V127":                   strings.Repeat("a", 127),
		"V128":                   strings.Repeat("b", 128),
		strings.Repeat("K", 200): "long name",
		"HUGE":                   strings.Repeat("c", 70000),
		strings.Repeat("N", 127): strings.Repeat("d", 128),
		"QUERY_STRING":           "a=1&b=2",
		"HTTP_X_UNICODE_日本":      "値",
	}
	got, err := decodePairs(encodePairs(m))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(m) {
		t.Fatalf("decoded %d pairs, want %d", len(got), len(m))
	}
	for k, v := range m {
		if got[k] != v {
			t.Errorf("%.20s: got %d bytes, want %d", k, len(got[k]), len(v))
		}
	}
}

func TestDecodePairsMalformed(t *testing.T) {
	for name, b := range map[string][]byte{
		"name length cut in its 4 bytes":  {0x80, 0, 1},
		"value length missing":            {3},
		"value length cut in its 4 bytes": {3, 0x80, 0},
		"pair shorter than its lengths":   {3, 2, 'a', 'b', 'c', 'x'},
		"4-byte lengths past the end":     {0x80, 0, 0, 200, 0x80, 0, 0, 200, 'x'},
	} {
		if _, err := decodePairs(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// fcgiTestServer runs Serve with h on a TCP listener, until the test ends.
func fcgiTestServer(t *testing.T, h Handler) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return l.Addr().String()
}

// fcgiRawConn dials addr and frames records with the server's own code,
// which reads and writes either side of the protocol.
func fcgiRawConn(t *testing.T, addr string) *serverConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return fcgiRawConnFrom(conn)
}

func (c *serverConn) fcgiSend(t *testing.T, typ uint8, id uint16, content []byte) {
	t.Helper()
	if err := c.writeRecord(typ, id, content); err != nil {
		t.Fatal(err)
	}
	if err := c.flush(); err != nil {
		t.Fatal(err)
	}
}

func (c *serverConn) fcgiBegin(t *testing.T, id, role uint16, keep bool) {
	t.Helper()
	b := []byte{0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b, role)
	if keep {
		b[2] = flagKeepConn
	}
	c.fcgiSend(t, typeBeginRequest, id, b)
}

// fcgiWriteRaw writes b as is, bypassing the record framing, and with
// closeWrite then closes the sending side.
func (c *serverConn) fcgiWriteRaw(t *testing.T, b []byte, closeWrite bool) {
	t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		t.Fatal(err)
	}
	if !closeWrite {
		return
	}
	tc, ok := c.conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("connection is %T, want *net.TCPConn", c.conn)
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
}

// fcgiResult is what a responder sent for one request.
type fcgiResult struct {
	stdout, stderr string
	appStatus      uint32
	protocolStatus uint8
}

// fcgiReadResult reads records for id until its FCGI_END_REQUEST.
func (c *serverConn) fcgiReadResult(t *testing.T, id uint16) fcgiResult {
	t.Helper()
	var res fcgiResult
	var stdoutEnded, stderrEnded bool
	for {
		rec, err := c.readRecord()
		if err != nil {
			t.Fatalf("reading the response: %v", err)
		}
		if rec.h.ID != id {
			t.Fatalf("record for request %d, want %d", rec.h.ID, id)
		}
		switch rec.h.Type {
		case typeStdout:
			res.stdout += string(rec.content)
			stdoutEnded = len(rec.content) == 0
		case typeStderr:
			res.stderr += string(rec.content)
			stderrEnded = len(rec.content) == 0
		case typeEndRequest:
			res.appStatus = binary.BigEndian.Uint32(rec.content)
			res.protocolStatus = rec.content[4]
			if res.protocolStatus == protocolStatusRequestComplete && (!stdoutEnded || !stderrEnded) {
				t.Errorf("FCGI_END_REQUEST before empty FCGI_STDOUT and FCGI_STDERR")
			}
			return res
		default:
			t.Fatalf("unexpected record type %d", rec.h.Type)
		}
	}
}

// fcgiExpectClosed checks that the server closed the connection.
func (c *serverConn) fcgiExpectClosed(t *testing.T) {
	t.Helper()
	if rec, err := c.readRecord(); err == nil {
		t.Fatalf("got record type %d, want the connection closed", rec.h.Type)
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatal("the connection stayed open")
	}
}

// fcgiEcho answers with its params and body, and writes to stderr.
func fcgiEcho(_ context.Context, r *Request) int {
	body, err := io.ReadAll(r.Stdin)
	if err != nil {
		_, _ = io.WriteString(r.Stderr, "stdin: "+err.Error())
		return 2
	}
	// A failed write shows as a response the test does not expect.
	_, _ = io.WriteString(r.Stdout, "Content-Type: text/plain\r\n\r\n")
	_, _ = io.WriteString(r.Stdout, r.Params["NAME"]+"|"+strconv.Itoa(len(r.Params["LONG"]))+"|"+string(body))
	_, _ = io.WriteString(r.Stderr, "logged")
	status, _ := strconv.Atoi(r.Params["STATUS"])
	return status
}

func TestServeRequest(t *testing.T) {
	addr := fcgiTestServer(t, fcgiEcho)
	c := fcgiRawConn(t, addr)
	c.fcgiBegin(t, 7, roleResponder, false)
	// Params split in the middle of a pair, across records.
	pairs := encodePairs(map[string]string{"NAME": "gophper", "LONG": strings.Repeat("x", 128), "STATUS": "3"})
	c.fcgiSend(t, typeParams, 7, pairs[:5])
	c.fcgiSend(t, typeParams, 7, pairs[5:])
	c.fcgiSend(t, typeParams, 7, nil)
	// The body split across records.
	for _, part := range []string{"hello ", "fast", "cgi"} {
		c.fcgiSend(t, typeStdin, 7, []byte(part))
	}
	c.fcgiSend(t, typeStdin, 7, nil)

	res := c.fcgiReadResult(t, 7)
	if want := "Content-Type: text/plain\r\n\r\ngophper|128|hello fastcgi"; res.stdout != want {
		t.Errorf("stdout = %q, want %q", res.stdout, want)
	}
	if res.stderr != "logged" || res.appStatus != 3 || res.protocolStatus != protocolStatusRequestComplete {
		t.Errorf("stderr %q, app status %d, protocol status %d", res.stderr, res.appStatus, res.protocolStatus)
	}
	// Without FCGI_KEEP_CONN, the server closes the connection.
	c.fcgiExpectClosed(t)
}

func TestServeKeepConn(t *testing.T) {
	addr := fcgiTestServer(t, fcgiEcho)
	c := fcgiRawConn(t, addr)
	for i := range 3 {
		id := uint16(i + 1)
		c.fcgiBegin(t, id, roleResponder, true)
		c.fcgiSend(t, typeParams, id, encodePairs(map[string]string{"NAME": strconv.Itoa(i)}))
		c.fcgiSend(t, typeParams, id, nil)
		c.fcgiSend(t, typeStdin, id, nil)
		res := c.fcgiReadResult(t, id)
		if !strings.HasSuffix(res.stdout, "\r\n\r\n"+strconv.Itoa(i)+"|0|") {
			t.Errorf("request %d: %q", i, res.stdout)
		}
	}
}

func TestServeManagementRecords(t *testing.T) {
	addr := fcgiTestServer(t, fcgiEcho)
	c := fcgiRawConn(t, addr)

	c.fcgiSend(t, typeGetValues, 0, encodePairs(map[string]string{"FCGI_MPXS_CONNS": ""}))
	rec, err := c.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	values, err := decodePairs(rec.content)
	if err != nil || rec.h.Type != typeGetValuesResult || values["FCGI_MPXS_CONNS"] != "0" {
		t.Errorf("FCGI_GET_VALUES: type %d, %v, %v", rec.h.Type, values, err)
	}

	// An unknown management record gets FCGI_UNKNOWN_TYPE, naming its type.
	c.fcgiSend(t, 99, 0, []byte("whatever"))
	rec, err = c.readRecord()
	if err != nil {
		t.Fatal(err)
	}
	if rec.h.Type != typeUnknownType || rec.h.ID != 0 || len(rec.content) != 8 || rec.content[0] != 99 {
		t.Errorf("unknown type: type %d, id %d, content %x", rec.h.Type, rec.h.ID, rec.content)
	}

	// The connection still serves a request afterwards.
	c.fcgiBegin(t, 1, roleResponder, false)
	c.fcgiSend(t, typeParams, 1, nil)
	c.fcgiSend(t, typeStdin, 1, nil)
	if res := c.fcgiReadResult(t, 1); res.protocolStatus != protocolStatusRequestComplete {
		t.Errorf("protocol status %d", res.protocolStatus)
	}
}

func TestServeRefusals(t *testing.T) {
	addr := fcgiTestServer(t, fcgiEcho)

	t.Run("unknown role", func(t *testing.T) {
		c := fcgiRawConn(t, addr)
		c.fcgiBegin(t, 1, 2, false) // FCGI_AUTHORIZER
		res := c.fcgiReadResult(t, 1)
		if res.protocolStatus != protocolStatusUnknownRole {
			t.Errorf("protocol status %d, want %d", res.protocolStatus, protocolStatusUnknownRole)
		}
	})

	t.Run("a second request on the connection", func(t *testing.T) {
		c := fcgiRawConn(t, addr)
		c.fcgiBegin(t, 1, roleResponder, false)
		c.fcgiBegin(t, 2, roleResponder, false)
		if res := c.fcgiReadResult(t, 2); res.protocolStatus != protocolStatusCantMpxConn {
			t.Errorf("protocol status %d, want %d", res.protocolStatus, protocolStatusCantMpxConn)
		}
		// Records for another request id are ignored, and the first one completes.
		c.fcgiSend(t, typeParams, 2, encodePairs(map[string]string{"NAME": "other"}))
		c.fcgiSend(t, typeData, 1, []byte("filter data is ignored"))
		c.fcgiSend(t, typeParams, 1, encodePairs(map[string]string{"NAME": "first"}))
		c.fcgiSend(t, typeParams, 1, nil)
		c.fcgiSend(t, typeStdin, 1, []byte("body"))
		c.fcgiSend(t, typeStdin, 1, nil)
		if res := c.fcgiReadResult(t, 1); !strings.HasSuffix(res.stdout, "first|0|body") {
			t.Errorf("stdout %q", res.stdout)
		}
	})
}

func TestServeMalformed(t *testing.T) {
	var calls sync.WaitGroup
	addr := fcgiTestServer(t, func(ctx context.Context, r *Request) int {
		calls.Done()
		return 0
	})
	for name, send := range map[string]func(t *testing.T, c *serverConn){
		"bad version": func(t *testing.T, c *serverConn) {
			c.fcgiWriteRaw(t, []byte{2, typeBeginRequest, 0, 1, 0, 8, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, false)
		},
		"short FCGI_BEGIN_REQUEST": func(t *testing.T, c *serverConn) {
			c.fcgiSend(t, typeBeginRequest, 1, []byte{0, 1})
		},
		"bad params": func(t *testing.T, c *serverConn) {
			c.fcgiBegin(t, 1, roleResponder, true)
			c.fcgiSend(t, typeParams, 1, []byte{0x80, 0, 0, 200, 1, 'x'})
			c.fcgiSend(t, typeParams, 1, nil)
		},
		"content cut short": func(t *testing.T, c *serverConn) {
			c.fcgiWriteRaw(t, []byte{1, typeParams, 0, 1, 0, 100, 0, 0, 'x'}, true)
		},
		"header cut short": func(t *testing.T, c *serverConn) {
			c.fcgiWriteRaw(t, []byte{1, typeParams, 0}, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := fcgiRawConn(t, addr)
			send(t, c)
			c.fcgiExpectClosed(t)
		})
	}

	// The server still serves requests.
	calls.Add(1)
	c := fcgiRawConn(t, addr)
	c.fcgiBegin(t, 1, roleResponder, false)
	c.fcgiSend(t, typeParams, 1, nil)
	c.fcgiSend(t, typeStdin, 1, nil)
	if res := c.fcgiReadResult(t, 1); res.protocolStatus != protocolStatusRequestComplete {
		t.Errorf("protocol status %d", res.protocolStatus)
	}
	calls.Wait()
}

func TestServeAbortRequest(t *testing.T) {
	started := make(chan struct{})
	addr := fcgiTestServer(t, func(ctx context.Context, r *Request) int {
		close(started)
		select {
		case <-ctx.Done():
			_, _ = io.WriteString(r.Stdout, "aborted")
			return 9
		case <-time.After(10 * time.Second):
			return 0
		}
	})
	c := fcgiRawConn(t, addr)
	c.fcgiBegin(t, 1, roleResponder, false)
	// An abort before the handler started does nothing.
	c.fcgiSend(t, typeAbortRequest, 1, nil)
	c.fcgiSend(t, typeParams, 1, nil)
	<-started
	c.fcgiSend(t, typeAbortRequest, 1, nil)
	res := c.fcgiReadResult(t, 1)
	if res.stdout != "aborted" || res.appStatus != 9 {
		t.Errorf("stdout %q, app status %d", res.stdout, res.appStatus)
	}
}

// A web server that disconnects mid-request cancels the handler, whose
// body then ends with an error, and its writes fail.
func TestServeClientGone(t *testing.T) {
	type result struct {
		ctxErr, readErr, writeErr error
	}
	got := make(chan result, 1)
	started := make(chan struct{})
	addr := fcgiTestServer(t, func(ctx context.Context, r *Request) int {
		close(started)
		_, readErr := io.ReadAll(r.Stdin)
		<-ctx.Done()
		var writeErr error
		// Larger than any buffer, so that a write reaches the closed socket.
		chunk := make([]byte, 1<<20)
		for range 50 {
			if _, writeErr = r.Stdout.Write(chunk); writeErr != nil {
				break
			}
		}
		got <- result{ctx.Err(), readErr, writeErr}
		return 0
	})
	c := fcgiRawConn(t, addr)
	c.fcgiBegin(t, 1, roleResponder, true)
	c.fcgiSend(t, typeParams, 1, nil)
	c.fcgiSend(t, typeStdin, 1, []byte("part of the body"))
	<-started
	if err := c.conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if !errors.Is(r.ctxErr, context.Canceled) || !errors.Is(r.readErr, io.ErrUnexpectedEOF) || r.writeErr == nil {
			t.Errorf("ctx %v, read %v, write %v", r.ctxErr, r.readErr, r.writeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was not canceled")
	}
}

// A body the handler does not read does not hold the connection.
func TestServeUnreadBody(t *testing.T) {
	addr := fcgiTestServer(t, func(_ context.Context, r *Request) int {
		_, _ = io.WriteString(r.Stdout, "done")
		return 0
	})
	c := fcgiRawConn(t, addr)
	c.fcgiBegin(t, 1, roleResponder, true)
	c.fcgiSend(t, typeParams, 1, nil)
	if res := c.fcgiReadResult(t, 1); res.stdout != "done" {
		t.Fatalf("stdout %q", res.stdout)
	}
	// The rest of the body arrives after the response.
	c.fcgiSend(t, typeStdin, 1, []byte("late"))
	c.fcgiSend(t, typeStdin, 1, nil)
	c.fcgiBegin(t, 2, roleResponder, false)
	c.fcgiSend(t, typeParams, 2, nil)
	if res := c.fcgiReadResult(t, 2); res.stdout != "done" {
		t.Errorf("second request: %q", res.stdout)
	}
}

// fcgiFlakyListener fails Accept with a timeout once, then with errs.
type fcgiFlakyListener struct {
	net.Listener
	mu   sync.Mutex
	errs []error
}

func (l *fcgiFlakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		return nil, err
	}
	return l.Listener.Accept()
}

type fcgiTimeoutError struct{}

func (fcgiTimeoutError) Error() string   { return "timeout" }
func (fcgiTimeoutError) Timeout() bool   { return true }
func (fcgiTimeoutError) Temporary() bool { return true }

func TestServeListenerErrors(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	broken := errors.New("broken listener")
	fl := &fcgiFlakyListener{Listener: l, errs: []error{fcgiTimeoutError{}, broken}}
	// A timeout is retried, and any other error ends Serve.
	if err := Serve(context.Background(), fl, fcgiEcho); !errors.Is(err, broken) {
		t.Errorf("Serve = %v, want %v", err, broken)
	}
}

func TestServeLargeOutput(t *testing.T) {
	const size = 3*maxContentLength + 10
	addr := fcgiTestServer(t, func(_ context.Context, r *Request) int {
		n, err := r.Stdout.Write(bytes.Repeat([]byte("y"), size))
		if n != size || err != nil {
			return 1
		}
		return 0
	})
	c := fcgiRawConn(t, addr)
	c.fcgiBegin(t, 1, roleResponder, false)
	c.fcgiSend(t, typeParams, 1, nil)
	c.fcgiSend(t, typeStdin, 1, nil)
	res := c.fcgiReadResult(t, 1)
	if len(res.stdout) != size || res.appStatus != 0 {
		t.Errorf("stdout %d bytes, app status %d", len(res.stdout), res.appStatus)
	}
}

// TestServeStopsWithIdleConnection keeps a connection open with no request,
// as nginx's keep-alive does, and stops the server. Serve must return.
func TestServeStopsWithIdleConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, func(context.Context, *Request) int { return 0 }) }()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(50 * time.Millisecond) // Accepted, and waiting for a record.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return while a connection was idle")
	}
}

// TestServeIdle closes a connection that waits too long: after a kept-alive
// request, and for params after FCGI_BEGIN_REQUEST. A request that runs
// longer keeps it.
func TestServeIdle(t *testing.T) {
	// Restored once the server is gone: cleanups run last first.
	old := serverIdle
	t.Cleanup(func() { serverIdle = old })
	serverIdle = 200 * time.Millisecond
	addr := fcgiTestServer(t, func(ctx context.Context, r *Request) int {
		if r.Params["SLOW"] != "" {
			time.Sleep(3 * serverIdle)
		}
		return fcgiEcho(ctx, r)
	})
	request := func(c *serverConn, params map[string]string) {
		t.Helper()
		c.fcgiBegin(t, 1, roleResponder, true)
		c.fcgiSend(t, typeParams, 1, encodePairs(params))
		c.fcgiSend(t, typeParams, 1, nil)
		c.fcgiSend(t, typeStdin, 1, nil)
		if res := c.fcgiReadResult(t, 1); res.protocolStatus != protocolStatusRequestComplete {
			t.Fatalf("protocol status %d", res.protocolStatus)
		}
	}
	c := fcgiRawConn(t, addr)
	request(c, map[string]string{"SLOW": "1"})
	request(c, map[string]string{"NAME": "after a slow one"})
	c.fcgiExpectClosed(t)

	c = fcgiRawConn(t, addr)
	c.fcgiBegin(t, 1, roleResponder, true)
	c.fcgiExpectClosed(t)
}

// TestServeParamsLimit drops a connection whose params grow past the cap.
func TestServeParamsLimit(t *testing.T) {
	c := fcgiRawConn(t, fcgiTestServer(t, fcgiEcho))
	c.fcgiBegin(t, 1, roleResponder, true)
	chunk := make([]byte, 65535)
	for range serverMaxParams/len(chunk) + 1 {
		if c.writeRecord(typeParams, 1, chunk) != nil || c.flush() != nil {
			break // closed already
		}
	}
	c.fcgiExpectClosed(t)
}
