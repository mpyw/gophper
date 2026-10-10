//go:build windows

//declscope:namespace socket

package hostnet

import (
	"os"
	"testing"
	"time"
)

// TestSocketPipeCloseLateRead closes a read end whose read starts only
// after the first cancel. Close alone would wait on that read for ever,
// as nothing writes. It is cancelled again, and the end closes.
func TestSocketPipeCloseLateRead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }() // only kept open, so the read blocks
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-started
		_, _ = r.Read(make([]byte, 1)) // cancelled
	}()
	closed := make(chan error, 1)
	go func() { closed <- socketPipeClose(r, done) }()
	// After the first cancel, which found no read.
	time.Sleep(3 * socketPipeCancelEvery)
	close(started)
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read end did not close")
	}
	if socketPipeClose(nil, nil) == nil {
		t.Error("closed no file")
	}
}

// TestSocketPipeCloseWhileReading closes a pipe read ahead, many times,
// as PHP's end of a pipe is when the request ends.
func TestSocketPipeCloseWhileReading(t *testing.T) {
	for range 200 {
		h := newSocketHarness(t)
		if errno := h.x.pipeOpen(h.ctx, 1, 2); errno != 0 {
			t.Fatalf("pipe_open = %d", errno)
		}
		h.tab.mu.Lock()
		h.tab.startReading(h.tab.entries[1])
		h.tab.mu.Unlock()
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			h.tab.Close()
		}()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("Close hangs on a pipe read ahead")
		}
	}
}
