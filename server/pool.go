package server

import (
	"bytes"
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
	// network and memoryLimit go to each instance's Options.
	network     bool
	memoryLimit int64
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

// poolEnforcedINI comes after the user's entries, so that it wins.
// FastCGIServer checks the file php-cgi walks SCRIPT_FILENAME back to.
// With cgi.fix_pathinfo=0, php-cgi would take PATH_TRANSLATED instead,
// which nothing checks.
var poolEnforcedINI = []string{"cgi.fix_pathinfo=1"}

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

// poolWorkersDir is where a worker sees the directory of the workers'
// sockets. A host path would not do: on Linux the sockets are under /tmp,
// which inside PHP is TempDir.
const poolWorkersDir = "/var/run/gophper/workers"

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
	// PHP sees it at /tmp whatever its host path, so its real path costs
	// nothing. wazero failed to read a nested mount, such as a project
	// under /tmp, below a root that was a symlink, as macOS's /tmp is.
	if real, err := filepath.EvalSymlinks(tempDir); err == nil {
		tempDir = real
	}
	if cfg.MemoryLimit < 0 {
		return nil, fmt.Errorf("memory limit %d is negative", cfg.MemoryLimit)
	}
	if err := poolCheckMounts(cfg.Mounts); err != nil {
		return nil, err
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
	// The enforced entries go before the user's [PATH=] and [HOST=]
	// sections, which would hold them otherwise.
	user, sections := cfg.INI, []string(nil)
	if i := slices.IndexFunc(cfg.INI, iniSpecialSection); i >= 0 {
		user, sections = cfg.INI[:i], cfg.INI[i:]
	}
	files["php.ini"] = strings.Join(slices.Concat(poolINI, opcacheINI, user, poolEnforcedINI, sections), "\n") + "\n"
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
		engine:      engine,
		fs:          fs,
		mounts:      cfg.Mounts,
		tempDir:     tempDir,
		opcacheDir:  opcacheDir,
		processes:   !cfg.NoProcesses,
		network:     !cfg.NoNetwork,
		memoryLimit: cfg.MemoryLimit,
		env: slices.DeleteFunc(slices.Clone(cfg.Env), func(kv string) bool {
			// GOPHPER_ names are gophper's own, set by the engine and the router.
			return poolVariable(kv, poolRequestVariables) || poolVariable(kv, poolStartVariables) || strings.HasPrefix(kv, "GOPHPER_")
		}),
		iniDir:    iniDir,
		sem:       make(chan struct{}, concurrency),
		maxWait:   cfg.MaxWaitTime,
		errorLog:  cfg.ErrorLog,
		accessLog: cfg.AccessLog,
		started:   time.Now(),
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
	if err := p.checkStart(); err != nil {
		return nil, errors.Join(err, p.close())
	}
	return p, nil
}

// poolCheckMounts checks that each mount is an absolute path to a
// directory.
//
//declscope:shared // http.go checks them before its own paths
func poolCheckMounts(mounts []Mount) error {
	for _, m := range mounts {
		if !filepath.IsAbs(m.Dir) {
			return fmt.Errorf("mount %q: not an absolute path", m.Dir)
		}
		if fi, err := os.Stat(m.Dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("mount %q: not a directory", m.Dir)
		}
	}
	return nil
}

// poolRequestVariables are a request's own CGI variables. Each request sets
// them, so one in Env does nothing there. php-cgi takes itself for a CGI
// request when one of the first four is set, and skips its own arguments,
// -b too, for a query string that starts with "-": a worker would not
// listen.
//
// REDIRECT_URL makes php-cgi run PATH_TRANSLATED instead of SCRIPT_FILENAME,
// past the script a request named.
var poolRequestVariables = []string{"SERVER_SOFTWARE", "SERVER_NAME", "GATEWAY_INTERFACE", "REQUEST_METHOD", "QUERY_STRING", "REDIRECT_URL"}

// poolStartVariables change how php-cgi starts: PHPRC and PHP_INI_SCAN_DIR
// would load another php.ini over the pool's own, cgi.fix_pathinfo=1
// included. PHP_FCGI_CHILDREN makes a worker fork, which WASI cannot, and a
// bad PHP_FCGI_BACKLOG stops it from listening. php-fpm does not let a
// pool's env[] change these either.
var poolStartVariables = []string{"PHPRC", "PHP_INI_SCAN_DIR", "PHP_FCGI_CHILDREN", "PHP_FCGI_BACKLOG"}

// poolVariable reports whether "KEY=VALUE" sets one of names.
func poolVariable(kv string, names []string) bool {
	k, _, _ := strings.Cut(kv, "=")
	return slices.Contains(names, k)
}

// checkStart starts php-cgi -v as each instance starts: its php.ini and
// mounts, under its memory cap. Opcache's shared memory, 64 MB for a
// worker, may not fit the cap, and then every request would fail. It also
// compiles php-cgi.wasm, which on a cold cache took most of a worker's
// time to listen. Engine.Compile would compile php.wasm too, which the
// server never runs.
func (p *pool) checkStart() error {
	var out bytes.Buffer
	code, err := p.engine.RunCGI(context.Background(), gophper.Options{
		Args: []string{"-v"}, Env: p.env, Stdout: &out, Stderr: &out,
		FS: p.fs, HostPath: p.hostPath, MemoryLimit: p.memoryLimit,
	})
	what := "php-cgi"
	if p.memoryLimit > 0 {
		what = fmt.Sprintf("memory limit %d", p.memoryLimit)
	}
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", what, err)
	case code != 0:
		return fmt.Errorf("%s: PHP cannot start: %s", what, strings.TrimSpace(out.String()))
	}
	return nil
}

// mounted reports whether a host path is inside one of the mounts.
//
//declscope:shared // http.go checks the document root, fastcgi.go each script
func (p *pool) mounted(path string) bool {
	return slices.ContainsFunc(p.mounts, func(m Mount) bool { return m.contains(path) })
}

// mountSpelling writes a host path inside a mount with the mount's own
// spelling. Windows matches paths without case, so C:\App\x.php is inside
// C:\app, but PHP finds it only at the mount's guest path, /c/app/x.php.
//
//declscope:shared // http.go spells the document root with it, fastcgi.go each script
func (p *pool) mountSpelling(path string) string {
	best, rel := "", ""
	for _, m := range p.mounts {
		if r, err := filepath.Rel(m.Dir, path); err == nil && m.contains(path) && len(m.Dir) > len(best) {
			best, rel = m.Dir, r
		}
	}
	if best == "" {
		return path
	}
	return filepath.Join(best, rel)
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
		// A fresh php-cgi takes the request's variables as its environment,
		// so a web server's FastCGI params could change how it starts. No
		// environment holds a name with "=" or NUL: PHPRC=/x: would be read
		// as PHPRC, and SCRIPT_FILENAME=/up.jpg/ would replace the script.
		if !slices.Contains(poolStartVariables, k) && !strings.ContainsAny(k, "=\x00") {
			env = append(env, k+"="+v)
		}
	}
	return p.engine.RunCGI(ctx, gophper.Options{
		Env: env, Stdin: stdin, Stdout: stdout, Stderr: stderr, FS: p.fs,
		HostPath: p.hostPath, Processes: p.processes, Network: p.network, MemoryLimit: p.memoryLimit,
	})
}

// hostPath maps a path inside PHP to the host, as the mounts do. The mount
// with the longest path wins, as wasi-libc picks among its preopens: a
// project under /tmp is its own mount, not TempDir's.
func (p *pool) hostPath(guest string) (string, bool, bool) {
	path, ok := hostpath.CleanGuest(guest)
	if !ok {
		return "", false, false
	}
	type poolPlace struct {
		guest, host string
		writable    bool
	}
	places := []poolPlace{{guest: "/tmp", host: p.tempDir, writable: true}}
	if p.opcacheDir != "" {
		places = append(places, poolPlace{guest: poolOpcacheDir, host: p.opcacheDir, writable: true})
	}
	if p.workers != nil {
		// A worker binds its FastCGI socket here.
		places = append(places, poolPlace{guest: poolWorkersDir, host: p.workers.dir, writable: true})
	}
	for _, m := range p.mounts {
		places = append(places, poolPlace{guest: hostpath.Guest(m.Dir), host: m.Dir, writable: !m.ReadOnly})
	}
	best, rest := -1, ""
	// The last of equal length wins, as in the FS config.
	for i, pl := range places {
		r, ok := strings.CutPrefix(path, pl.guest)
		if !ok || (r != "" && r[0] != '/' && pl.guest != "/") {
			continue
		}
		if best < 0 || len(pl.guest) >= len(places[best].guest) {
			best, rest = i, r
		}
	}
	if best < 0 {
		return "", false, false
	}
	pl := places[best]
	host := filepath.Join(pl.host, filepath.FromSlash(rest))
	// path is clean, so this holds. It is checked anyway: a path that left
	// its mount would reach the host outside the sandbox.
	if rel, err := filepath.Rel(pl.host, host); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, false
	}
	return host, pl.writable, true
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
