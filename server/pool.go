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
)

// pool runs php-cgi for FastCGIServer and HTTPHandler: one fresh instance
// per request, at most Concurrency at a time, with the same mounts and
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
	files["php.ini"] = strings.Join(slices.Concat(poolINI, cfg.INI), "\n") + "\n"
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(iniDir, name), []byte(content), 0o644); err != nil {
			os.RemoveAll(iniDir)
			return nil, err
		}
	}

	fs := wazero.NewFSConfig()
	for _, m := range cfg.Mounts {
		if m.ReadOnly {
			fs = fs.WithReadOnlyDirMount(m.Dir, m.Dir)
		} else {
			fs = fs.WithDirMount(m.Dir, m.Dir)
		}
	}
	fs = fs.WithDirMount(tempDir, "/tmp").WithReadOnlyDirMount(iniDir, poolBootstrapDir)

	p := &pool{
		engine:    engine,
		fs:        fs,
		mounts:    cfg.Mounts,
		env:       cfg.Env,
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

	env := slices.Clone(p.env)
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return p.engine.RunCGI(ctx, gophper.Options{Env: env, Stdin: stdin, Stdout: stdout, Stderr: stderr, FS: p.fs})
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
	fmt.Fprintf(p.accessLog, "%s - - [%s] %q %d %d %.3fs\n",
		remote, start.Format("02/Jan/2006:15:04:05 -0700"), method+" "+uri+" "+proto, status, size, time.Since(start).Seconds())
}

//declscope:shared // http.go and fastcgi.go
func (p *pool) close() error {
	return os.RemoveAll(p.iniDir)
}
