package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

// pool runs php-cgi for FastCGIServer and HTTPHandler: one fresh
// instance per request, at most workers at a time, with the same mounts and
// php.ini for every request.
//
//declscope:shared // fastcgi.go and http.go serve requests through it
type pool struct {
	//declscope:private
	engine *gophper.Engine
	//declscope:private
	fs wazero.FSConfig
	//declscope:private
	iniDir string
	//declscope:private
	sem chan struct{}
}

// poolINI comes before the user's entries. It replaces what php-cgi would
// otherwise need from the environment, so nothing extra shows in getenv()
// or $_SERVER.
var poolINI = []string{
	// Instead of REDIRECT_STATUS. php-cgi is never reached directly here.
	"cgi.force_redirect=0",
	// Instead of TMPDIR. The temporary directory is mounted there.
	"sys_temp_dir=/tmp",
}

// newPool mounts root at the same path, tempDir at /tmp and the php.ini
// at /etc/gophper. Empty tempDir means os.TempDir(), and zero workers means
// runtime.NumCPU().
//
//declscope:shared // fastcgi.go and http.go
func newPool(engine *gophper.Engine, root, tempDir string, workers int, ini []string) (*pool, error) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	// php-cgi reads /etc/gophper/php.ini by default (--with-config-file-path),
	// so no PHPRC is needed. It is not passed through -d either: php-cgi
	// skips its arguments when QUERY_STRING starts with "-".
	iniDir, err := os.MkdirTemp("", "gophper-ini-")
	if err != nil {
		return nil, err
	}
	content := strings.Join(slices.Concat(poolINI, ini), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(iniDir, "php.ini"), []byte(content), 0o644); err != nil {
		os.RemoveAll(iniDir)
		return nil, err
	}
	return &pool{
		engine: engine,
		fs: wazero.NewFSConfig().
			WithDirMount(root, root).
			WithDirMount(tempDir, "/tmp").
			WithReadOnlyDirMount(iniDir, "/etc/gophper"),
		iniDir: iniDir,
		sem:    make(chan struct{}, workers),
	}, nil
}

// run runs one request. env is the complete CGI environment.
//
//declscope:shared // fastcgi.go and http.go
func (p *pool) run(ctx context.Context, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return p.engine.RunCGI(ctx, gophper.Options{Env: env, Stdin: stdin, Stdout: stdout, Stderr: stderr, FS: p.fs})
}

//declscope:shared // fastcgi.go and http.go
func (p *pool) close() error {
	return os.RemoveAll(p.iniDir)
}
