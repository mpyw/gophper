package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// TestPoolHostPathStaysInside maps paths with ".." in them. None may leave
// the mount it starts in: a sandboxed script could chmod any host file.
func TestPoolHostPathStaysInside(t *testing.T) {
	p, vars := poolForTest(t)
	root := filepath.Dir(vars["SCRIPT_FILENAME"])
	for _, guest := range []string{
		"/tmp/../../../etc/passwd",
		"/tmp/../" + filepath.ToSlash(root[1:]) + "/../../outside",
		gophper.HostToGuest(root) + "/../outside",
		"relative/path",
	} {
		host, _, ok := p.hostPath(guest)
		if !ok {
			continue
		}
		inside := false
		for _, dir := range []string{p.tempDir, root} {
			if rel, err := filepath.Rel(dir, host); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				inside = true
			}
		}
		if !inside {
			t.Errorf("%q mapped outside every mount: %q", guest, host)
		}
	}
}

func TestPoolMountSpelling(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "Nested")
	p := &pool{mounts: []Mount{{Dir: root}, {Dir: nested}}}
	tests := map[string]string{
		filepath.Join(root, "a", "x.php"):   filepath.Join(root, "a", "x.php"),
		filepath.Join(nested, "x.php"):      filepath.Join(nested, "x.php"),
		filepath.Dir(root):                  filepath.Dir(root),
		"":                                  "",
		filepath.Join(root, "a", "..", "b"): filepath.Join(root, "b"),
	}
	if runtime.GOOS == "windows" {
		// Windows matches paths without case: the mount's spelling wins.
		tests[strings.ToUpper(filepath.Join(nested, "x.php"))] = filepath.Join(nested, "X.PHP")
	}
	for in, want := range tests {
		if got := p.mountSpelling(in); got != want {
			t.Errorf("mountSpelling(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPoolRunCanceledWhileWaiting gives up waiting for a free instance when
// the request goes away.
func TestPoolRunCanceledWhileWaiting(t *testing.T) {
	p, vars := poolForTest(t)
	for range cap(p.sem) {
		p.sem <- struct{}{}
	}
	defer func() {
		for range cap(p.sem) {
			<-p.sem
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.run(ctx, vars, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestPoolLogAccess(t *testing.T) {
	var log bytes.Buffer
	p := &pool{accessLog: &log}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p.logAccess("", "GET", "/x", "HTTP/1.1", 200, 5, start)
	p.logAccess("192.0.2.1:1234", "POST", "/y", "HTTP/2.0", 404, 0, start)
	want := []string{
		`- - - [02/Jan/2026:03:04:05 +0000] "GET /x HTTP/1.1" 200 5 `,
		`192.0.2.1 - - [02/Jan/2026:03:04:05 +0000] "POST /y HTTP/2.0" 404 0 `,
	}
	lines := strings.Split(strings.TrimSuffix(log.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], want[0]) || !strings.HasPrefix(lines[1], want[1]) {
		t.Errorf("logged %q", log.String())
	}
}

func TestINISpecialSection(t *testing.T) {
	for line, want := range map[string]bool{
		"[PATH=/srv]":    true,
		"[host=example]": true,
		`[ "PATH=/a b"]`: true,
		"['HOST=x']":     true,
		"[PHP]":          false,
		"[ab]":           false,
		"not a section":  false,
		"[unterminated":  false,
	} {
		if got := iniSpecialSection(line); got != want {
			t.Errorf("iniSpecialSection(%q) = %v, want %v", line, got, want)
		}
	}
}

// TestPoolConfigErrors fails where the opcache or temporary directory
// cannot be made.
func TestPoolConfigErrors(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := PHPConfig{Mounts: []Mount{{Dir: t.TempDir()}}, TempDir: t.TempDir(), ErrorLog: io.Discard}
	cfg.OpcacheDir = file
	if p, err := newPool(engine, cfg, nil); err == nil || !strings.Contains(err.Error(), "opcache directory") {
		if p != nil {
			_ = p.close()
		}
		t.Errorf("opcache directory in a file: %v", err)
	}
	if runtime.GOOS != "windows" {
		// No user cache directory for the default opcache directory.
		cfg.OpcacheDir = ""
		t.Setenv("HOME", "")
		t.Setenv("XDG_CACHE_HOME", "")
		if p, err := newPool(engine, cfg, nil); err == nil || !strings.Contains(err.Error(), "opcache directory") {
			if p != nil {
				_ = p.close()
			}
			t.Errorf("no user cache directory: %v", err)
		}
	}
	// A memory cap PHP cannot start under, or less than nothing.
	for _, limit := range []int64{1 << 20, -1} {
		capped := cfg
		capped.NoOpcache, capped.MemoryLimit = true, limit
		if p, err := newPool(engine, capped, nil); err == nil || !strings.Contains(err.Error(), "memory limit") {
			if p != nil {
				_ = p.close()
			}
			t.Errorf("MemoryLimit %d: %v", limit, err)
		}
	}
	cfg.NoOpcache = true
	t.Setenv("TMPDIR", filepath.Join(file, "tmp"))
	t.Setenv("TMP", filepath.Join(file, "tmp"))
	if p, err := newPool(engine, cfg, nil); err == nil {
		_ = p.close()
		t.Error("no temporary directory: no error")
	}
}

// TestPoolWorkerStartTimeout gives up on a worker that does not listen in
// time, and stops it.
func TestPoolWorkerStartTimeout(t *testing.T) {
	// Restored once the pool is gone: cleanups run last first.
	old := poolWorkerStartTimeout
	t.Cleanup(func() { poolWorkerStartTimeout = old })
	p, vars := poolForTest(t)
	poolWorkerStartTimeout = time.Nanosecond
	_, err := p.run(context.Background(), vars, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not listen in time") {
		t.Errorf("err = %v", err)
	}
	if len(p.workers.all) != 0 {
		t.Errorf("%d workers left", len(p.workers.all))
	}
}
