package server

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/internal/fcgi"
)

// FastCGIServer serves PHP over FastCGI, like php-fpm.
//
// FCGI_PARAMS from the web server become the CGI environment of php-cgi
// unchanged, and nothing else is added. The host environment is not
// inherited (php-fpm's clear_env).
type FastCGIServer struct {
	Engine *gophper.Engine
	// Root is the host directory PHP may access. It is mounted at the same
	// path inside PHP, so SCRIPT_FILENAME from the web server works as is.
	Root string
	// TempDir is mounted at /tmp inside PHP. Empty means os.TempDir().
	TempDir string
	// Workers limits concurrent PHP instances (php-fpm's pm.max_children).
	// Zero means runtime.NumCPU().
	Workers int
	// INI holds php.ini lines such as "max_execution_time=30".
	INI []string
}

// Serve accepts FastCGI connections on l until ctx is done.
func (s *FastCGIServer) Serve(ctx context.Context, l net.Listener) error {
	cgi, err := newPool(s.Engine, s.Root, s.TempDir, s.Workers, s.INI)
	if err != nil {
		return err
	}
	defer cgi.close()

	return fcgi.Serve(ctx, l, func(ctx context.Context, r *fcgi.Request) int {
		env := make([]string, 0, len(r.Params))
		for k, v := range r.Params {
			env = append(env, k+"="+v)
		}
		out := &fastcgiStdout{w: r.Stdout}
		code, err := cgi.run(ctx, env, r.Stdin, out, r.Stderr)
		if err != nil {
			fmt.Fprintf(r.Stderr, "gophper: %s: %v\n", r.Params["SCRIPT_FILENAME"], err)
			if out.n == 0 {
				fmt.Fprint(r.Stdout, "Status: 500 Internal Server Error\r\nContent-Type: text/plain\r\n\r\n500 Internal Server Error\n")
			}
			if code == 0 {
				code = 1
			}
		}
		return code
	})
}

// fastcgiStdout is a request's FCGI_STDOUT that counts the bytes written.
// Serve sends its own error response only while nothing was written yet.
type fastcgiStdout struct {
	w io.Writer
	n int64
}

func (c *fastcgiStdout) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
