package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/internal/hostpath"
)

// pool runs php-cgi for FastCGIServer and HTTPHandler: on workers that
// serve one request after another, or with NoWorkers on a fresh instance
// per request. At most Concurrency run at a time, with the same mounts and
// php.ini for every request.
//
//declscope:shared // http.go and fastcgi.go serve requests through it
type pool struct {
	//declscope:private
	engine *gophper.Engine
	//declscope:private
	fs wazero.FSConfig
	//declscope:private
	mounts []Mount
	//declscope:private
	tempDir string
	//declscope:private
	opcacheDir string
	//declscope:private
	processes bool
	//declscope:private
	env []string
	//declscope:private
	iniDir string
	//declscope:private
	sem chan struct{}
	//declscope:private
	maxWait time.Duration
	// errorLog and accessLog are never nil.
	errorLog, accessLog io.Writer
	//declscope:private
	active, total atomic.Int64
	//declscope:private
	started time.Time
	// workers is nil when each request starts its own instance.
	//
	//declscope:private
	workers *poolWorkers
}

// errPoolBusy reports a request that waited MaxWaitTime for an instance.
//
//declscope:shared // http.go and fastcgi.go answer it with 503
var errPoolBusy = errors.New("no PHP instance became free within the maximum wait time")

// poolINI comes before the user's entries. It replaces what php-cgi would
// otherwise need from the environment, so nothing extra shows in getenv()
// or $_SERVER.
var poolINI = []string{
	// Instead of REDIRECT_STATUS. php-cgi is never reached directly here.
	"cgi.force_redirect=0",
	// Instead of TMPDIR. The temporary directory is mounted there.
	"sys_temp_dir=/tmp",
	// As php.ini-production and php.ini-development set it. PHP's built-in
	// default also fills $_ENV, and getenv() then returns a copy taken at
	// startup, which putenv() cannot change.
	"variables_order=GPCS",
}

// poolMaxRequests is how many requests a worker serves before it is
// replaced, as php-cgi's own PHP_FCGI_MAX_REQUESTS default.
const poolMaxRequests = 500

// poolOpcacheDir is where OpcacheDir is mounted.
const poolOpcacheDir = "/var/cache/gophper/opcache"

// poolOpcacheINI turns on opcache with its file cache.
var poolOpcacheINI = []string{
	"opcache.enable=1",
	"opcache.file_cache=" + poolOpcacheDir,
}

// poolOpcacheWorkerINI adds shared memory for workers, which keep it from
// one request to the next: Laravel then takes 23 ms instead of 84. Each
// worker has its own, so it is smaller than PHP's 128 MB default.
var poolOpcacheWorkerINI = []string{
	"opcache.file_cache_only=0",
	"opcache.memory_consumption=64",
}

// poolOpcacheInstanceINI keeps a fresh instance to the file cache: its
// shared memory would be thrown away with it.
var poolOpcacheInstanceINI = []string{
	"opcache.file_cache_only=1",
}

// poolBootstrapDir is where php.ini and the extra files are mounted.
const poolBootstrapDir = "/etc/gophper"

// newPool prepares a pool. files are mounted read-only beside php.ini, for
// scripts the server runs itself. See bootstrapPath.
//
//declscope:shared // http.go and fastcgi.go
func newPool(engine *gophper.Engine, cfg PHPConfig, files map[string]string) (*pool, error) {
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	tempDir := cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	for _, m := range cfg.Mounts {
		if !filepath.IsAbs(m.Dir) {
			return nil, fmt.Errorf("mount %q: not an absolute path", m.Dir)
		}
		if fi, err := os.Stat(m.Dir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("mount %q: not a directory", m.Dir)
		}
	}

	var opcacheDir string
	// php-cgi enables opcache by default, so NoOpcache must say so.
	opcacheINI := []string{"opcache.enable=0"}
	if !cfg.NoOpcache {
		opcacheDir = cfg.OpcacheDir
		if opcacheDir == "" {
			base, err := os.UserCacheDir()
			if err != nil {
				return nil, fmt.Errorf("opcache directory: %w", err)
			}
			opcacheDir = filepath.Join(base, "gophper", "opcache")
		}
		// opcache's system id does not tell two builds of one PHP release
		// apart. A script compiled by the other would be rejected, compiled
		// again on every request, and never written over.
		opcacheDir = filepath.Join(opcacheDir, engine.BuildID())
		// Compiled scripts are code: only this user may write them.
		if err := os.MkdirAll(opcacheDir, 0o700); err != nil {
			return nil, fmt.Errorf("opcache directory: %w", err)
		}
		opcacheINI = slices.Concat(poolOpcacheINI, poolOpcacheInstanceINI)
		if !cfg.NoWorkers {
			opcacheINI = slices.Concat(poolOpcacheINI, poolOpcacheWorkerINI)
		}
	}

	// php-cgi reads /etc/gophper/php.ini by default (--with-config-file-path),
	// so no PHPRC is needed. It is not passed through -d either: php-cgi
	// skips its arguments when QUERY_STRING starts with "-".
	iniDir, err := os.MkdirTemp("", "gophper-")
	if err != nil {
		return nil, err
	}
	files = maps.Clone(files)
	if files == nil {
		files = map[string]string{}
	}
	files["php.ini"] = strings.Join(slices.Concat(poolINI, opcacheINI, cfg.INI), "\n") + "\n"
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(iniDir, name), []byte(content), 0o644); err != nil {
			_ = os.RemoveAll(iniDir) // the write error is the one to report
			return nil, err
		}
	}

	fs := wazero.NewFSConfig()
	for _, m := range cfg.Mounts {
		if m.ReadOnly {
			fs = fs.WithReadOnlyDirMount(m.Dir, hostpath.Guest(m.Dir))
		} else {
			fs = fs.WithDirMount(m.Dir, hostpath.Guest(m.Dir))
		}
	}
	fs = fs.WithDirMount(tempDir, "/tmp").WithReadOnlyDirMount(iniDir, poolBootstrapDir)
	if opcacheDir != "" {
		fs = fs.WithDirMount(opcacheDir, poolOpcacheDir)
	}

	p := &pool{
		engine:     engine,
		fs:         fs,
		mounts:     cfg.Mounts,
		tempDir:    tempDir,
		opcacheDir: opcacheDir,
		processes:  !cfg.NoProcesses,
		env:        cfg.Env,
		iniDir:     iniDir,
		sem:        make(chan struct{}, concurrency),
		maxWait:    cfg.MaxWaitTime,
		errorLog:   cfg.ErrorLog,
		accessLog:  cfg.AccessLog,
		started:    time.Now(),
	}
	if p.errorLog == nil {
		p.errorLog = os.Stderr
	}
	if p.accessLog == nil {
		p.accessLog = io.Discard
	}
	if !cfg.NoWorkers {
		// Only this user may reach the workers' sockets.
		dir, err := os.MkdirTemp("", "gophper-w")
		if err != nil {
			_ = os.RemoveAll(iniDir) // the MkdirTemp error is the one to report
			return nil, err
		}
		maxRequests := cfg.MaxRequests
		if maxRequests <= 0 {
			maxRequests = poolMaxRequests
		}
		p.workers = &poolWorkers{dir: dir, maxRequests: maxRequests, all: map[*poolWorker]struct{}{}}
	}
	return p, nil
}

// mounted reports whether a host path is inside one of the mounts.
//
//declscope:shared // http.go checks the document root, fastcgi.go each script
func (p *pool) mounted(path string) bool {
	return slices.ContainsFunc(p.mounts, func(m Mount) bool { return m.contains(path) })
}

// bootstrapPath is where PHP sees one of the files newPool was given.
//
//declscope:shared // http.go runs its router bootstrap from there
func (p *pool) bootstrapPath(name string) string {
	return poolBootstrapDir + "/" + name
}

// poolStats are the counters the FastCGI status page shows.
//
//declscope:shared // fastcgi.go prints them
type poolStats struct {
	started          time.Time
	accepted, active int64
	maxActive        int
}

//declscope:shared // fastcgi.go
func (p *pool) stats() poolStats {
	return poolStats{started: p.started, accepted: p.total.Load(), active: p.active.Load(), maxActive: cap(p.sem)}
}

// run runs one request. vars are the CGI variables, which override Env.
//
//declscope:shared // http.go and fastcgi.go
func (p *pool) run(ctx context.Context, vars map[string]string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var timeout <-chan time.Time
	if p.maxWait > 0 {
		t := time.NewTimer(p.maxWait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-timeout:
		return 0, errPoolBusy
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	p.active.Add(1)
	p.total.Add(1)
	defer p.active.Add(-1)
	if p.workers != nil {
		return p.poolServe(ctx, vars, stdin, stdout, stderr)
	}

	env := slices.Clone(p.env)
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return p.engine.RunCGI(ctx, gophper.Options{
		Env: env, Stdin: stdin, Stdout: stdout, Stderr: stderr, FS: p.fs,
		HostPath: p.hostPath, Processes: p.processes,
	})
}

// hostPath maps a path inside PHP to the host, as the mounts do.
func (p *pool) hostPath(path string) (string, bool, bool) {
	if rel, ok := strings.CutPrefix(path, "/tmp"); ok && (rel == "" || rel[0] == '/') {
		return filepath.Join(p.tempDir, rel), true, true
	}
	if rel, ok := strings.CutPrefix(path, poolOpcacheDir); ok && p.opcacheDir != "" && (rel == "" || rel[0] == '/') {
		return filepath.Join(p.opcacheDir, rel), true, true
	}
	host, ok := hostpath.Host(path)
	if !ok {
		return "", false, false
	}
	// The last mount wins, as in the FS config.
	for _, m := range slices.Backward(p.mounts) {
		if m.contains(host) {
			return host, !m.ReadOnly, true
		}
	}
	// A worker binds its FastCGI socket here, outside every mount.
	if p.workers != nil && (Mount{Dir: p.workers.dir}).contains(host) {
		return host, true, true
	}
	return "", false, false
}

// logAccess writes a Common Log Format line, plus the time taken.
//
//declscope:shared // http.go and fastcgi.go
func (p *pool) logAccess(remote, method, uri, proto string, status int, size int64, start time.Time) {
	if p.accessLog == io.Discard {
		return
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if remote == "" {
		remote = "-"
	}
	writePoolLog(p.accessLog, "%s - - [%s] %q %d %d %.3fs\n",
		remote, start.Format("02/Jan/2006:15:04:05 -0700"), method+" "+uri+" "+proto, status, size, time.Since(start).Seconds())
}

// writePoolLog writes one line to an error log, an access log or FCGI_STDERR.
// A failed write to a log has nowhere to be reported, so its error is dropped.
//
//declscope:shared // fastcgi.go, http.go and worker.go
func writePoolLog(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

//declscope:shared // http.go and fastcgi.go
func (p *pool) close() error {
	var err error
	if p.workers != nil {
		p.poolStopWorkers()
		err = os.RemoveAll(p.workers.dir)
	}
	return errors.Join(err, os.RemoveAll(p.iniDir))
}
