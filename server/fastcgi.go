package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/internal/fcgi"
)

// FastCGIServer serves PHP over FastCGI, like php-fpm.
//
// FCGI_PARAMS from the web server become the CGI environment of php-cgi
// unchanged, on top of PHPConfig.Env. Which script runs is the web
// server's choice, through SCRIPT_FILENAME.
type FastCGIServer struct {
	pool    *pool
	cfg     FastCGIConfig
	clients []netip.Prefix
}

// NewFastCGIServer prepares a server. Close it to remove its php.ini.
func NewFastCGIServer(engine *gophper.Engine, cfg FastCGIConfig) (*FastCGIServer, error) {
	if len(cfg.Mounts) == 0 {
		return nil, errors.New("no mounts: PHP could reach no script")
	}
	if len(cfg.LimitExtensions) == 0 {
		cfg.LimitExtensions = []string{".php", ".phar"}
	}
	s := &FastCGIServer{cfg: cfg}
	for _, c := range cfg.AllowedClients {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			addr, aerr := netip.ParseAddr(c)
			if aerr != nil {
				return nil, fmt.Errorf("allowed client %q: not an address or prefix", c)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		s.clients = append(s.clients, prefix)
	}
	p, err := newPool(engine, cfg.PHPConfig, nil)
	if err != nil {
		return nil, err
	}
	s.pool = p
	return s, nil
}

// Close releases the server. Serve must have returned.
func (s *FastCGIServer) Close() error {
	return s.pool.close()
}

// Serve accepts FastCGI connections on l until ctx is done.
func (s *FastCGIServer) Serve(ctx context.Context, l net.Listener) error {
	if len(s.clients) > 0 {
		l = &fastcgiListener{Listener: l, allowed: s.clients, errorLog: s.pool.errorLog}
	}
	return fcgi.Serve(ctx, l, s.handle)
}

func (s *FastCGIServer) handle(ctx context.Context, r *fcgi.Request) int {
	start := time.Now()
	out := &fastcgiStdout{w: r.Stdout}
	defer func() {
		s.pool.logAccess(r.Params["REMOTE_ADDR"], r.Params["REQUEST_METHOD"], r.Params["REQUEST_URI"],
			r.Params["SERVER_PROTOCOL"], out.status(), out.n, start)
	}()

	uri := r.Params["REQUEST_URI"]
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	switch {
	case s.cfg.PingPath != "" && (uri == s.cfg.PingPath || r.Params["SCRIPT_NAME"] == s.cfg.PingPath):
		fmt.Fprint(out, "Content-Type: text/plain\r\n\r\npong\n")
		return 0
	case s.cfg.StatusPath != "" && (uri == s.cfg.StatusPath || r.Params["SCRIPT_NAME"] == s.cfg.StatusPath):
		st := s.pool.stats()
		fmt.Fprintf(out, "Content-Type: text/plain\r\n\r\nstart time: %s\nstart since: %d\naccepted requests: %d\nactive processes: %d\nmax active processes: %d\n",
			st.started.Format(time.RFC1123Z), int(time.Since(st.started).Seconds()), st.accepted, st.active, st.maxActive)
		return 0
	}

	script := r.Params["SCRIPT_FILENAME"]
	if !slices.Contains(s.cfg.LimitExtensions, filepath.Ext(script)) {
		fmt.Fprintf(r.Stderr, "gophper: access to the script %q has been denied (see LimitExtensions)\n", script)
		fmt.Fprint(out, "Status: 403 Forbidden\r\nContent-Type: text/plain\r\n\r\nAccess denied.\n")
		return 1
	}
	if !s.pool.mounted(script) {
		fmt.Fprintf(r.Stderr, "gophper: %q is outside every mount\n", script)
		fmt.Fprint(out, "Status: 404 Not Found\r\nContent-Type: text/plain\r\n\r\nFile not found.\n")
		return 1
	}

	code, err := s.pool.run(ctx, r.Params, r.Stdin, out, r.Stderr)
	if err != nil {
		fmt.Fprintf(r.Stderr, "gophper: %s: %v\n", script, err)
		if out.n == 0 {
			status := "500 Internal Server Error"
			if errors.Is(err, errPoolBusy) {
				status = "503 Service Unavailable"
			}
			fmt.Fprintf(out, "Status: %s\r\nContent-Type: text/plain\r\n\r\n%s\n", status, status)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}

// fastcgiStdout is a request's FCGI_STDOUT. It counts the bytes written, so
// that handle sends its own error response only while nothing was written,
// and keeps the start of the headers for the access log.
type fastcgiStdout struct {
	w    io.Writer
	n    int64
	head []byte
}

func (c *fastcgiStdout) Write(p []byte) (int, error) {
	if len(c.head) < 4096 {
		c.head = append(c.head, p[:min(len(p), 4096-len(c.head))]...)
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// status reads the Status header PHP sent, as a web server would.
func (c *fastcgiStdout) status() int {
	head, _, _ := bytes.Cut(c.head, []byte("\r\n\r\n"))
	for line := range strings.SplitSeq(string(head), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Status:"); ok {
			if code, err := strconv.Atoi(strings.Fields(v + " 0")[0]); err == nil {
				return code
			}
		}
	}
	return 200
}

// fastcgiListener drops TCP connections from addresses not allowed.
type fastcgiListener struct {
	net.Listener
	allowed  []netip.Prefix
	errorLog io.Writer
}

func (l *fastcgiListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return conn, nil
		}
		addr := tcp.AddrPort().Addr().Unmap()
		if slices.ContainsFunc(l.allowed, func(p netip.Prefix) bool { return p.Contains(addr) }) {
			return conn, nil
		}
		fmt.Fprintf(l.errorLog, "gophper: connection from %s is not allowed\n", addr)
		conn.Close()
	}
}
