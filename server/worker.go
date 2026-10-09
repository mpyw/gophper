//declscope:namespace pool

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
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
}

// poolWorker is one running php-cgi.
type poolWorker struct {
	sock   string
	cancel context.CancelFunc
	// served counts requests. Only the request holding w changes it.
	served int
	// done is closed when php-cgi exits.
	done chan struct{}
}

// poolWorkerStartTimeout is how long php-cgi gets to start listening.
const poolWorkerStartTimeout = 30 * time.Second

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
		conn, err := net.Dial("unix", w.sock)
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
	w := &poolWorker{sock: sock, cancel: cancel, done: make(chan struct{})}
	env := append(slices.Clone(p.env), "PHP_FCGI_MAX_REQUESTS=0")
	go func() {
		defer close(w.done)
		_, err := p.engine.RunCGI(ctx, gophper.Options{
			Args: []string{"-b", sock}, Env: env, Stdout: p.errorLog, Stderr: p.errorLog, FS: p.fs,
			HostPath: p.hostPath, Processes: p.processes,
		})
		if err != nil && ctx.Err() == nil {
			writePoolLog(p.errorLog, "gophper: worker: %v\n", err)
		}
	}()
	p.workers.mu.Lock()
	p.workers.all[w] = struct{}{}
	p.workers.mu.Unlock()

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
	p.workers.mu.Unlock()
	go func() {
		<-w.done
		_ = os.Remove(w.sock) // php-cgi may have removed it, or never made it
	}()
}

// poolStopWorkers stops every worker and waits for them.
func (p *pool) poolStopWorkers() {
	p.workers.mu.Lock()
	all := make([]*poolWorker, 0, len(p.workers.all))
	for w := range p.workers.all {
		all = append(all, w)
	}
	p.workers.all = map[*poolWorker]struct{}{}
	p.workers.idle = nil
	p.workers.mu.Unlock()
	for _, w := range all {
		w.cancel()
		<-w.done
	}
}
