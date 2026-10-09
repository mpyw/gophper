package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper"
)

// poolForTest returns a pool of workers for a directory holding index.php,
// which prints "ok", and the CGI variables that run it. A pool runs paths
// as PHP sees them.
func poolForTest(t *testing.T) (*pool, map[string]string) {
	t.Helper()
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.php"), []byte(`<?php echo "ok";`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := newPool(engine, PHPConfig{
		Mounts:      []Mount{{Dir: root}},
		TempDir:     t.TempDir(),
		Concurrency: 1,
		NoOpcache:   true,
		ErrorLog:    io.Discard,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.close()
		_ = engine.Close(context.Background())
	})
	return p, map[string]string{"SCRIPT_FILENAME": gophper.HostToGuest(filepath.Join(root, "index.php")), "REQUEST_METHOD": "GET"}
}

// poolRunForTest runs index.php and checks its output.
func (p *pool) poolRunForTest(t *testing.T, vars map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	code, err := p.run(ctx, vars, nil, &out, io.Discard)
	if err != nil || code != 0 || !strings.HasSuffix(out.String(), "\r\n\r\nok") {
		t.Fatalf("code %d, err %v, output %q", code, err, out.String())
	}
}

// poolIdleForTest returns the idle workers.
func (p *pool) poolIdleForTest() []*poolWorker {
	p.workers.mu.Lock()
	defer p.workers.mu.Unlock()
	return append([]*poolWorker(nil), p.workers.idle...)
}

func (p *pool) poolKnowsForTest(w *poolWorker) bool {
	p.workers.mu.Lock()
	defer p.workers.mu.Unlock()
	_, ok := p.workers.all[w]
	return ok
}

// A worker that exits while idle is replaced on the next request.
func TestPoolWorkerExitedWhileIdle(t *testing.T) {
	p, vars := poolForTest(t)
	p.poolRunForTest(t, vars)
	idle := p.poolIdleForTest()
	if len(idle) != 1 {
		t.Fatalf("%d idle workers, want 1", len(idle))
	}
	dead := idle[0]
	dead.cancel()
	<-dead.done

	p.poolRunForTest(t, vars)
	if p.poolKnowsForTest(dead) {
		t.Error("the exited worker is still in the pool")
	}
	if _, err := os.Stat(dead.sock); !os.IsNotExist(err) {
		t.Errorf("its socket: %v", err)
	}
	if idle := p.poolIdleForTest(); len(idle) != 1 || idle[0] == dead {
		t.Errorf("idle workers %v, want one new worker", idle)
	}
}

// A worker that exits right after its request is not made idle.
func TestPoolPutExitedWorker(t *testing.T) {
	p, vars := poolForTest(t)
	p.poolRunForTest(t, vars)
	w, err := p.poolTake()
	if err != nil {
		t.Fatal(err)
	}
	w.cancel()
	<-w.done
	p.poolPut(w)
	if len(p.poolIdleForTest()) != 0 || p.poolKnowsForTest(w) {
		t.Error("the exited worker went back to the pool")
	}
	p.poolRunForTest(t, vars)
}

// A worker whose socket refuses connections is dropped, and the request
// tries a fresh one. A second refusal fails the request.
func TestPoolWorkerRefuses(t *testing.T) {
	p, vars := poolForTest(t)
	refusing := func() *poolWorker {
		done := make(chan struct{})
		w := &poolWorker{sock: filepath.Join(t.TempDir(), "gone.sock"), cancel: func() { close(done) }, done: done}
		p.workers.mu.Lock()
		p.workers.idle = append(p.workers.idle, w)
		p.workers.all[w] = struct{}{}
		p.workers.mu.Unlock()
		return w
	}

	w := refusing()
	p.poolRunForTest(t, vars)
	if p.poolKnowsForTest(w) {
		t.Error("the refusing worker is still in the pool")
	}

	// Idle workers are taken last in, first out.
	p.workers.mu.Lock()
	p.workers.idle = nil
	p.workers.mu.Unlock()
	refusing()
	refusing()
	_, err := p.run(context.Background(), vars, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "worker: dial unix") {
		t.Errorf("err = %v, want a dial error", err)
	}
}

func TestPoolWorkerCannotListen(t *testing.T) {
	p, vars := poolForTest(t)
	p.workers.dir = filepath.Join(t.TempDir(), "missing")
	_, err := p.run(context.Background(), vars, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "php-cgi exited before it listened") {
		t.Errorf("err = %v", err)
	}
	if len(p.workers.all) != 0 {
		t.Errorf("%d workers left", len(p.workers.all))
	}
}

// TestPoolStopWaitsForStopping stops a worker, as a canceled request does,
// then closes the pool at once. Closing must wait for that worker too: the
// engine closing under a running php-cgi crashed in wazero.
func TestPoolStopWaitsForStopping(t *testing.T) {
	p, vars := poolForTest(t)
	p.poolRunForTest(t, vars)
	idle := p.poolIdleForTest()
	if len(idle) != 1 {
		t.Fatalf("%d idle workers, want 1", len(idle))
	}
	w := idle[0]
	p.poolStop(w)
	p.poolStopWorkers()
	select {
	case <-w.done:
	default:
		t.Fatal("poolStopWorkers returned while a stopping worker still ran")
	}
	if _, err := p.poolStart(filepath.Join(t.TempDir(), "late.sock")); err == nil {
		t.Error("a worker started after poolStopWorkers")
	}
}
