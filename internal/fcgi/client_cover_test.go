//declscope:namespace fcgi

package fcgi

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// When ctx ends and conn takes no deadline, Do closes conn to wake its
// reader.
func TestDoAbortCloses(t *testing.T) {
	client, server := fcgiPipe(t)
	read := make(chan struct{})
	responded := make(chan struct{})
	go func() {
		defer close(responded)
		c := fcgiRawConnFrom(server)
		c.fcgiReadRequest()
		close(read)
		// Never answers. The read ends when Do closes the connection.
		_, _ = c.readRecord()
	}()
	t.Cleanup(func() { <-responded })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-read
		cancel()
	}()
	conn := &fcgiFakeConn{Conn: client, deadlineErr: errors.New("no deadlines")}
	if _, err := Do(ctx, conn, nil, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A bad record ends Do while the request is still being sent: the
// responder answered before reading it.
func TestDoAnswerBeforeRequest(t *testing.T) {
	client, server := fcgiPipe(t)
	wrote := make(chan error, 1)
	go func() {
		_, err := server.Write([]byte{9, typeStdout, 0, 1, 0, 0, 0, 0})
		wrote <- err
	}()
	if _, err := Do(context.Background(), client, nil, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "unsupported version 9") {
		t.Errorf("err = %v, want an unsupported version", err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
}

// A request that fails to send ends Do with that error. The connection
// takes writes until it breaks, and Do's reader waits until the writer
// gives up.
func TestDoSendFails(t *testing.T) {
	// One FCGI_PARAMS record of this many bytes fills the 4096-byte write
	// buffer after FCGI_BEGIN_REQUEST's 16, header included. A one-byte
	// name, and a value long enough for a 4-byte length.
	const fillingValue = 4096 - 16 - 8 - 1 - 1 - 4
	for _, tc := range []struct {
		name   string
		writes int
		params map[string]string
		stdin  io.Reader
	}{
		{name: "params past the buffer", params: map[string]string{"A": strings.Repeat("v", 5000)}},
		{name: "params end past the buffer", params: map[string]string{"A": strings.Repeat("v", fillingValue)}},
		{name: "params flushed", params: map[string]string{"A": "1"}},
		{name: "body past the buffer", writes: 1, stdin: strings.NewReader(strings.Repeat("b", maxContentLength+1))},
		{name: "body flushed", writes: 1, stdin: strings.NewReader("b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := fcgiPipe(t)
			// The responder reads what gets through, until the test ends.
			go func() { _, _ = io.Copy(io.Discard, server) }() // ends with the pipe, as expected
			broken := errors.New("broken pipe")
			conn := &fcgiFakeConn{Conn: client, writes: tc.writes, writeErr: broken}
			_, err := Do(context.Background(), conn, tc.params, tc.stdin, io.Discard, io.Discard)
			if !errors.Is(err, broken) || !strings.Contains(err.Error(), "sending the request") {
				t.Errorf("err = %v, want %v", err, broken)
			}
		})
	}
}
