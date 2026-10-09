//declscope:namespace pool

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/internal/fcgi"
)

// Workers: each is one php-cgi in FastCGI mode, as a php-fpm child is. It
// serves requests one at a time on a Unix socket in poolWorkers.dir, and
// PHP resets its state between them. Starting an instance per request
// costs more than most requests.
//
// A worker that fails a request, or whose request is canceled, is stopped,
// so that no script runs on unseen. So is one that served maxRequests.
// The pool counts them: php-cgi's own PHP_FCGI_MAX_REQUESTS would close
// the socket under a request that was already connected.

// poolWorkers are the idle workers of a pool.
type poolWorkers struct {
	dir         string
	maxRequests int
	seq         int
	mu          sync.Mutex
	idle        []*poolWorker
	all         map[*poolWorker]struct{}
	// running counts the goroutines of every worker, stopping ones too,
	// so that poolStopWorkers returns only once no php-cgi runs. The
	// engine must not close under one. closed refuses new workers then.
	running sync.WaitGroup
	closed  bool
}

// poolWorker is one running php-cgi.
type poolWorker struct {
	sock   string
	cancel context.CancelFunc
	// served counts requests. Only the request holding w changes it.
	served int
	// done is closed when php-cgi exits.
	done chan struct{}
	// listenBy is when php-cgi must listen, until its first connection.
	// The socket file appears at bind(), a moment before listen(), so a
	// connection in between is refused. Zero once connected, or for a
	// worker that served before.
	listenBy time.Time
}

// poolWorkerStartTimeout is how long php-cgi gets to start listening. A
// variable, so that tests need not wait as long.
var poolWorkerStartTimeout = 30 * time.Second

// poolServe runs one request on a worker.
func (p *pool) poolServe(ctx context.Context, vars map[string]string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// A worker can exit between requests, such as after a fatal error in
	// PHP's own startup. Its socket then refuses the connection, and a fresh
	// worker is tried once.
	for attempt := 0; ; attempt++ {
		w, err := p.poolTake()
		if err != nil {
			return 0, err
		}
		conn, err := p.poolDial(w)
		if err != nil {
			p.poolStop(w)
			if attempt == 0 {
				continue
			}
			return 0, fmt.Errorf("worker: %w", err)
		}
		code, err := fcgi.Do(ctx, conn, vars, stdin, stdout, stderr)
		_ = conn.Close() // Do is done with it, and its result is all that matters
		if err != nil {
			p.poolStop(w)
			return code, err
		}
		if w.served++; w.served >= p.workers.maxRequests {
			p.poolStop(w)
		} else {
			p.poolPut(w)
		}
		return code, nil
	}
}

// poolDial connects to w. A worker that just started may not listen yet,
// so a refused connection is tried again until it must.
func (p *pool) poolDial(w *poolWorker) (net.Conn, error) {
	for {
		conn, err := net.Dial("unix", w.sock)
		// Windows refuses with WSAECONNREFUSED, which syscall does not name.
		refused := errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061))
		if err == nil || w.listenBy.IsZero() || !refused || time.Now().After(w.listenBy) {
			w.listenBy = time.Time{}
			return conn, err
		}
		select {
		case <-w.done:
			return nil, err
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// poolTake returns an idle worker, or starts one.
func (p *pool) poolTake() (*poolWorker, error) {
	ws := p.workers
	ws.mu.Lock()
	for len(ws.idle) > 0 {
		w := ws.idle[len(ws.idle)-1]
		ws.idle = ws.idle[:len(ws.idle)-1]
		select {
		case <-w.done:
			delete(ws.all, w)
			_ = os.Remove(w.sock) // php-cgi may have removed it
			continue
		default:
		}
		ws.mu.Unlock()
		return w, nil
	}
	ws.seq++
	sock := filepath.Join(ws.dir, strconv.Itoa(ws.seq)+".sock")
	ws.mu.Unlock()
	return p.poolStart(sock)
}

// poolStart starts php-cgi listening on sock, and waits until it listens.
func (p *pool) poolStart(sock string) (*poolWorker, error) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &poolWorker{sock: sock, cancel: cancel, done: make(chan struct{}), listenBy: time.Now().Add(poolWorkerStartTimeout)}
	env := append(slices.Clone(p.env), "PHP_FCGI_MAX_REQUESTS=0")
	p.workers.mu.Lock()
	if p.workers.closed {
		p.workers.mu.Unlock()
		cancel()
		return nil, errors.New("worker: the server is closed")
	}
	p.workers.all[w] = struct{}{}
	p.workers.running.Add(1)
	p.workers.mu.Unlock()
	go func() {
		defer p.workers.running.Done()
		defer close(w.done)
		_, err := p.engine.RunCGI(ctx, gophper.Options{
			Args: []string{"-b", path.Join(poolWorkersDir, filepath.Base(sock))}, Env: env, Stdout: p.errorLog, Stderr: p.errorLog, FS: p.fs,
			HostPath: p.hostPath, Processes: p.processes, Network: p.network, MemoryLimit: p.memoryLimit,
		})
		if err != nil && ctx.Err() == nil {
			writePoolLog(p.errorLog, "gophper: worker: %v\n", err)
		}
	}()

	deadline := time.After(poolWorkerStartTimeout)
	for {
		if _, err := os.Stat(sock); err == nil {
			return w, nil
		}
		select {
		case <-w.done:
			p.poolStop(w)
			return nil, errors.New("worker: php-cgi exited before it listened")
		case <-deadline:
			p.poolStop(w)
			return nil, errors.New("worker: php-cgi did not listen in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// poolPut makes w idle again, unless it has exited.
func (p *pool) poolPut(w *poolWorker) {
	select {
	case <-w.done:
		p.poolStop(w)
		return
	default:
	}
	p.workers.mu.Lock()
	p.workers.idle = append(p.workers.idle, w)
	p.workers.mu.Unlock()
}

// poolStop stops w and forgets it.
func (p *pool) poolStop(w *poolWorker) {
	w.cancel()
	p.workers.mu.Lock()
	delete(p.workers.all, w)
	p.workers.running.Add(1)
	p.workers.mu.Unlock()
	go func() {
		defer p.workers.running.Done()
		<-w.done
		_ = os.Remove(w.sock) // php-cgi may have removed it, or never made it
	}()
}

// poolStopWorkers stops every worker, and waits for them and for those
// already stopping.
func (p *pool) poolStopWorkers() {
	p.workers.mu.Lock()
	p.workers.closed = true
	for w := range p.workers.all {
		w.cancel()
	}
	p.workers.all = map[*poolWorker]struct{}{}
	p.workers.idle = nil
	p.workers.mu.Unlock()
	p.workers.running.Wait()
}
