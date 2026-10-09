package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

// fcgiResponse is a parsed FastCGI response.
type fcgiResponse struct {
	Status    int
	Header    textproto.MIMEHeader
	Body      string
	Stderr    string
	AppStatus uint32
}

// fcgiClient is a minimal FastCGI client, enough to drive the server in tests.
type fcgiClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialFCGI(t testing.TB, addr string) *fcgiClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &fcgiClient{conn: conn, r: bufio.NewReader(conn)}
}

func (c *fcgiClient) write(typ uint8, id uint16, content []byte) error {
	var b bytes.Buffer
	pad := -len(content) & 7
	binary.Write(&b, binary.BigEndian, struct {
		Version, Type uint8
		ID, Len       uint16
		Pad, Reserved uint8
	}{1, typ, id, uint16(len(content)), uint8(pad), 0})
	b.Write(content)
	b.Write(make([]byte, pad))
	_, err := c.conn.Write(b.Bytes())
	return err
}

func (c *fcgiClient) do(id uint16, keep bool, params map[string]string, body []byte) (*fcgiResponse, error) {
	if err := c.send(id, keep, params, body); err != nil {
		return nil, err
	}
	return c.read()
}

// send writes a request, with its body.
func (c *fcgiClient) send(id uint16, keep bool, params map[string]string, body []byte) error {
	flags := byte(0)
	if keep {
		flags = 1
	}
	if err := c.write(1, id, []byte{0, 1, flags, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	var p []byte
	for k, v := range params {
		for _, n := range []int{len(k), len(v)} {
			if n < 128 {
				p = append(p, byte(n))
			} else {
				p = binary.BigEndian.AppendUint32(p, uint32(n)|1<<31)
			}
		}
		p = append(p, k...)
		p = append(p, v...)
	}
	c.write(4, id, p)
	c.write(4, id, nil)
	for len(body) > 0 {
		n := min(len(body), 65535)
		c.write(5, id, body[:n])
		body = body[n:]
	}
	return c.write(5, id, nil)
}

// read reads a response up to FCGI_END_REQUEST.
func (c *fcgiClient) read() (*fcgiResponse, error) {
	var stdout, stderr bytes.Buffer
	for {
		var h struct {
			Version, Type uint8
			ID, Len       uint16
			Pad, Reserved uint8
		}
		if err := binary.Read(c.r, binary.BigEndian, &h); err != nil {
			return nil, err
		}
		buf := make([]byte, int(h.Len)+int(h.Pad))
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		buf = buf[:h.Len]
		switch h.Type {
		case 6:
			stdout.Write(buf)
		case 7:
			stderr.Write(buf)
		case 3:
			res := &fcgiResponse{AppStatus: binary.BigEndian.Uint32(buf), Stderr: stderr.String(), Status: 200}
			tp := textproto.NewReader(bufio.NewReader(&stdout))
			hdr, err := tp.ReadMIMEHeader()
			if err != nil {
				return nil, fmt.Errorf("bad CGI headers: %w\n%s", err, stdout.String())
			}
			res.Header = hdr
			if s := hdr.Get("Status"); s != "" {
				res.Status, _ = strconv.Atoi(strings.Fields(s)[0])
			}
			rest, _ := io.ReadAll(tp.R)
			res.Body = string(rest)
			return res, nil
		}
	}
}

func startFCGI(t testing.TB, ini ...string) (addr, root string) {
	t.Helper()
	return startFCGIWith(t, func(cfg *server.FastCGIConfig) { cfg.INI = ini })
}

func startFCGIWith(t testing.TB, configure func(*server.FastCGIConfig)) (addr, root string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs("testdata/www")
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.FastCGIConfig{PHPConfig: server.PHPConfig{
		Mounts:      []server.Mount{{Dir: root}},
		TempDir:     t.TempDir(),
		Concurrency: 4,
	}}
	configure(&cfg)
	srv, err := server.NewFastCGIServer(engine, cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, l); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
		engine.Close(context.Background())
	})
	return l.Addr().String(), root
}

// params mimics nginx's fastcgi_params with fastcgi_split_path_info.
func params(root, method, uri string, extra map[string]string) map[string]string {
	path, query, _ := strings.Cut(uri, "?")
	scriptName, pathInfo := path, ""
	if i := strings.Index(path, ".php"); i >= 0 {
		scriptName, pathInfo = path[:i+4], path[i+4:]
	}
	p := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_SOFTWARE":   "nginx",
		"SERVER_PROTOCOL":   "HTTP/1.1",
		"REQUEST_METHOD":    method,
		"REQUEST_URI":       uri,
		"QUERY_STRING":      query,
		"SCRIPT_NAME":       scriptName,
		"PATH_INFO":         pathInfo,
		"SCRIPT_FILENAME":   root + scriptName,
		"DOCUMENT_ROOT":     root,
		"SERVER_NAME":       "localhost",
		"SERVER_PORT":       "80",
		"REMOTE_ADDR":       "127.0.0.1",
		"HTTP_HOST":         "localhost",
		"HTTP_USER_AGENT":   "gophper-test",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestFastCGI(t *testing.T) {
	addr, root := startFCGI(t)

	t.Run("params reach $_SERVER unchanged", func(t *testing.T) {
		c := dialFCGI(t, addr)
		res, err := c.do(1, false, params(root, "GET", "/index.php/users/42?a=1&b[]=x", map[string]string{"GOPHPER_CUSTOM": "custom-value"}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != 201 || res.Header.Get("X-Gophper") != "yes" {
			t.Fatalf("status %d, header %v\n%s", res.Status, res.Header, res.Stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
			t.Fatalf("%v: %s", err, res.Body)
		}
		want := map[string]any{
			"sapi":        "cgi-fcgi",
			"script_name": "/index.php",
			"path_info":   "/users/42",
			"request_uri": "/index.php/users/42?a=1&b[]=x",
			"method":      "GET",
			"ua":          "gophper-test",
			"custom":      "custom-value",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %v, want %v", k, got[k], v)
			}
		}
		if fmt.Sprint(got["get"]) != "map[a:1 b:[x]]" {
			t.Errorf("get = %v", got["get"])
		}
	})

	t.Run("POST body larger than one record", func(t *testing.T) {
		c := dialFCGI(t, addr)
		form := "name=gophper&pad=" + strings.Repeat("x", 200_000)
		res, err := c.do(1, false, params(root, "POST", "/index.php", map[string]string{
			"CONTENT_TYPE":   "application/x-www-form-urlencoded",
			"CONTENT_LENGTH": strconv.Itoa(len(form)),
		}), []byte(form))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Post    map[string]string `json:"post"`
			BodyLen int               `json:"body_len"`
		}
		if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
			t.Fatalf("%v: %s", err, res.Body)
		}
		if got.Post["name"] != "gophper" || len(got.Post["pad"]) != 200_000 {
			t.Errorf("post = name %q, pad %d bytes", got.Post["name"], len(got.Post["pad"]))
		}
	})

	t.Run("keep-alive serves several requests on one connection", func(t *testing.T) {
		c := dialFCGI(t, addr)
		for i := range 3 {
			res, err := c.do(uint16(i+1), true, params(root, "GET", "/index.php?i="+strconv.Itoa(i), nil), nil)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			if res.Status != 201 {
				t.Fatalf("request %d: status %d", i, res.Status)
			}
		}
	})

	// Like php-fpm: with display_errors on, the error is the page and the status stays 200.
	t.Run("fatal error with display_errors on", func(t *testing.T) {
		c := dialFCGI(t, addr)
		res, err := c.do(1, false, params(root, "GET", "/index.php?mode=fatal", nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusOK || !strings.Contains(res.Body, "Call to undefined function undefined_fn()") {
			t.Errorf("status = %d, want 200 with the error in the body\n%s", res.Status, res.Body)
		}
	})

	t.Run("missing script", func(t *testing.T) {
		c := dialFCGI(t, addr)
		res, err := c.do(1, false, params(root, "GET", "/nope.php", nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusNotFound {
			t.Errorf("status = %d, want 404\n%s", res.Status, res.Body)
		}
	})

	t.Run("requests run in parallel", func(t *testing.T) {
		const n = 4
		start := time.Now()
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				c := dialFCGI(t, addr)
				res, err := c.do(1, false, params(root, "GET", "/index.php?mode=sleep&ms=500", nil), nil)
				if err != nil || res.Body != "slept\n" {
					t.Errorf("%v %+v", err, res)
				}
			})
		}
		wg.Wait()
		if d := time.Since(start); d > 1500*time.Millisecond {
			t.Errorf("%d requests of 500ms took %s; not parallel", n, d)
		}
	})
}

func TestFastCGIINI(t *testing.T) {
	addr, root := startFCGI(t, "display_errors=0", "log_errors=1")

	// The query starts with "-", which makes php-cgi skip -d arguments. PHPRC still applies.
	for _, uri := range []string{"/index.php?mode=fatal", "/index.php?-x&mode=fatal"} {
		c := dialFCGI(t, addr)
		res, err := c.do(1, false, params(root, "GET", uri, nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500\n%s", uri, res.Status, res.Body)
		}
		if strings.Contains(res.Body, "undefined_fn") {
			t.Errorf("%s: error leaked into the body:\n%s", uri, res.Body)
		}
		if !strings.Contains(res.Stderr, "Call to undefined function undefined_fn()") {
			t.Errorf("%s: error not logged to FCGI_STDERR:\n%s", uri, res.Stderr)
		}
	}
}

// Nothing the server needs leaks into the script's environment.
func TestFastCGIEnvironment(t *testing.T) {
	addr, root := startFCGI(t)
	res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/env.php", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env              map[string]bool `json:"env"`
		TempDir          string          `json:"temp_dir"`
		INIFile          string          `json:"ini_file"`
		MaxExecutionTime string          `json:"max_execution_time"`
	}
	if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
		t.Fatalf("%v: %s", err, res.Body)
	}
	for k, present := range got.Env {
		if present {
			t.Errorf("%s is visible to PHP", k)
		}
	}
	if got.TempDir != "/tmp" || got.INIFile != "/etc/gophper/php.ini" {
		t.Errorf("temp dir %q, ini file %q", got.TempDir, got.INIFile)
	}
	// php-cgi's built-in default, as with php-fpm.
	if got.MaxExecutionTime != "30" {
		t.Errorf("max_execution_time = %q, want 30", got.MaxExecutionTime)
	}
}

func TestFastCGITimeout(t *testing.T) {
	addr, root := startFCGI(t, "max_execution_time=1", "display_errors=0", "log_errors=1")

	for _, tc := range []struct{ name, uri string }{
		{"sleeping", "/index.php?mode=sleep&ms=5000&shutdown=1"},
		{"busy loop", "/index.php?mode=spin&shutdown=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dialFCGI(t, addr)
			start := time.Now()
			res, err := c.do(1, false, params(root, "GET", tc.uri, nil), nil)
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < time.Second || d > 2500*time.Millisecond {
				t.Errorf("timeout took %s, want about 1s", d)
			}
			if res.Status != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500\n%s", res.Status, res.Body)
			}
			if !strings.Contains(res.Stderr, "Maximum execution time of 1 second exceeded") {
				t.Errorf("stderr:\n%s", res.Stderr)
			}
			if !strings.Contains(res.Body, "shutdown ran") {
				t.Errorf("shutdown function did not run:\n%s", res.Body)
			}
		})
	}
}

func BenchmarkFastCGI(b *testing.B) {
	addr, root := startFCGI(b)
	p := params(root, "GET", "/index.php?a=1", nil)
	b.Run("sequential", func(b *testing.B) {
		c := dialFCGI(b, addr)
		for i := 0; b.Loop(); i++ {
			if _, err := c.do(uint16(i%65535+1), true, p, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			c := dialFCGI(b, addr)
			for i := 0; pb.Next(); i++ {
				if _, err := c.do(uint16(i%65535+1), true, p, nil); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}

func TestFastCGILimitExtensions(t *testing.T) {
	addr, root := startFCGI(t)
	p := params(root, "GET", "/env.php", nil)
	p["SCRIPT_FILENAME"] = root + "/env.txt"
	res, err := dialFCGI(t, addr).do(1, false, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusForbidden || !strings.Contains(res.Stderr, "has been denied") {
		t.Errorf("status %d\n%s\n%s", res.Status, res.Body, res.Stderr)
	}
}

func TestFastCGIOutsideMounts(t *testing.T) {
	addr, root := startFCGI(t)
	p := params(root, "GET", "/index.php", nil)
	p["SCRIPT_FILENAME"] = "/etc/passwd.php"
	res, err := dialFCGI(t, addr).do(1, false, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusNotFound {
		t.Errorf("status %d\n%s", res.Status, res.Body)
	}
}

func TestFastCGIPingStatusAndAccessLog(t *testing.T) {
	var log bytes.Buffer
	var mu sync.Mutex
	addr, root := startFCGIWith(t, func(cfg *server.FastCGIConfig) {
		cfg.PingPath, cfg.StatusPath = "/ping", "/status"
		cfg.AccessLog = writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return log.Write(p) })
	})
	for path, want := range map[string]string{"/ping": "pong\n", "/status": "accepted requests: 1\n"} {
		if path == "/status" {
			// One PHP request first, so the counter moves.
			if _, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/index.php", nil), nil); err != nil {
				t.Fatal(err)
			}
		}
		res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", path, nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Body, want) {
			t.Errorf("%s: %q", path, res.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !regexp.MustCompile(`127\.0\.0\.1 - - \[[^]]+\] "GET /index.php HTTP/1.1" 201 \d+ [0-9.]+s`).Match(log.Bytes()) {
		t.Errorf("access log:\n%s", log.String())
	}
}

func TestFastCGIAllowedClients(t *testing.T) {
	addr, root := startFCGIWith(t, func(cfg *server.FastCGIConfig) { cfg.AllowedClients = []string{"10.0.0.0/8"} })
	if _, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/index.php", nil), nil); err == nil {
		t.Error("a client outside AllowedClients got a response")
	}
}

func TestFastCGIConfigErrors(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cfg  server.FastCGIConfig
		want string
	}{
		{"no mounts", server.FastCGIConfig{}, "no mounts"},
		{"bad client", server.FastCGIConfig{
			PHPConfig:      server.PHPConfig{Mounts: []server.Mount{{Dir: dir}}},
			AllowedClients: []string{"10.0.0.0/8", "localhost"},
		}, `allowed client "localhost": not an address or prefix`},
		{"relative mount", server.FastCGIConfig{PHPConfig: server.PHPConfig{Mounts: []server.Mount{{Dir: "www"}}}}, `mount "www": not an absolute path`},
		{"mount is a file", server.FastCGIConfig{PHPConfig: server.PHPConfig{Mounts: []server.Mount{{Dir: file}}}}, "not a directory"},
		{"missing mount", server.FastCGIConfig{PHPConfig: server.PHPConfig{Mounts: []server.Mount{{Dir: filepath.Join(dir, "nope")}}}}, "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := server.NewFastCGIServer(engine, tc.cfg)
			if err == nil {
				srv.Close()
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// An address in AllowedClients allows that address only.
func TestFastCGIAllowedClientAddress(t *testing.T) {
	addr, root := startFCGIWith(t, func(cfg *server.FastCGIConfig) { cfg.AllowedClients = []string{"192.0.2.1", "127.0.0.1"} })
	res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/index.php", nil), nil)
	if err != nil || res.Status != 201 {
		t.Fatalf("%v %+v", err, res)
	}
}

// AllowedClients applies to TCP. A Unix socket is guarded by its file mode.
func TestFastCGIAllowedClientsUnix(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	root, err := filepath.Abs("testdata/www")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.NewFastCGIServer(engine, server.FastCGIConfig{
		PHPConfig:      server.PHPConfig{Mounts: []server.Mount{{Dir: root}}, TempDir: t.TempDir(), Concurrency: 1},
		AllowedClients: []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// Short, as a Unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "gfcgi")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	conn, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &fcgiClient{conn: conn, r: bufio.NewReader(conn)}
	res, err := c.do(1, false, params(root, "GET", "/index.php", nil), nil)
	if err != nil || res.Status != 201 {
		t.Fatalf("%v %+v", err, res)
	}
}

// startFCGIScripts serves a directory holding scripts, by file name, with
// one PHP instance.
func startFCGIScripts(t *testing.T, scripts map[string]string, configure func(*server.FastCGIConfig)) (addr, root string) {
	t.Helper()
	root = t.TempDir()
	for name, src := range scripts {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	addr, _ = startFCGIWith(t, func(cfg *server.FastCGIConfig) {
		cfg.Mounts = []server.Mount{{Dir: root}}
		cfg.Concurrency = 1
		cfg.ErrorLog = io.Discard
		configure(cfg)
	})
	return addr, root
}

// A worker killed in the middle of a request fails that request only. The
// next one runs on a fresh worker.
func TestFastCGIWorkerKilled(t *testing.T) {
	addr, root := startFCGIScripts(t, map[string]string{
		"kill.php":    `<?php posix_kill(getmypid(), SIGKILL);`,
		"partial.php": `<?php echo "partial"; flush(); posix_kill(getmypid(), SIGKILL);`,
		"ok.php":      `<?php echo "ok";`,
	}, func(*server.FastCGIConfig) {})
	ok := func() {
		t.Helper()
		res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/ok.php", nil), nil)
		if err != nil || res.Status != 200 || res.Body != "ok" {
			t.Fatalf("the next request: %v %+v", err, res)
		}
	}
	ok()

	res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/kill.php", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusInternalServerError || res.AppStatus == 0 || !strings.Contains(res.Stderr, "kill.php") {
		t.Errorf("killed: status %d, app status %d, stderr %q", res.Status, res.AppStatus, res.Stderr)
	}
	ok()

	// Once PHP sent output, the error cannot replace it.
	res, err = dialFCGI(t, addr).do(1, false, params(root, "GET", "/partial.php", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.Body != "partial" || res.AppStatus == 0 {
		t.Errorf("killed after output: status %d, body %q, app status %d", res.Status, res.Body, res.AppStatus)
	}
	ok()
}

// A request that waits longer than MaxWaitTime for the only instance gets 503.
func TestFastCGIBusy(t *testing.T) {
	addr, root := startFCGIScripts(t, map[string]string{
		"slow.php": `<?php usleep(300000); echo "slow";`,
		"ok.php":   `<?php echo "ok";`,
	}, func(cfg *server.FastCGIConfig) {
		cfg.MaxWaitTime = 20 * time.Millisecond
		cfg.StatusPath = "/status"
	})
	slow := make(chan *fcgiResponse, 1)
	go func() {
		res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/slow.php", nil), nil)
		if err != nil {
			t.Error(err)
		}
		slow <- res
	}()
	// Wait until it holds the instance.
	for deadline := time.Now().Add(5 * time.Second); ; {
		res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/status", nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Body, "active processes: 1\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slow request did not start:\n%s", res.Body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/ok.php", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusServiceUnavailable || !strings.Contains(res.Stderr, "maximum wait time") {
		t.Errorf("status %d, stderr %q", res.Status, res.Stderr)
	}
	if res := <-slow; res == nil || res.Body != "slow" {
		t.Errorf("the slow request: %+v", res)
	}
}

// FCGI_ABORT_REQUEST stops the script, and the worker is replaced.
func TestFastCGIAbort(t *testing.T) {
	addr, root := startFCGIScripts(t, map[string]string{
		"sleep.php": `<?php sleep(10); echo "finished";`,
		"ok.php":    `<?php echo "ok";`,
	}, func(cfg *server.FastCGIConfig) { cfg.StatusPath = "/status" })
	c := dialFCGI(t, addr)
	if err := c.send(1, false, params(root, "GET", "/sleep.php", nil), nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		res, err := dialFCGI(t, addr).do(1, false, params(root, "GET", "/status", nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Body, "active processes: 1\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := c.write(2, 1, nil); err != nil {
		t.Fatal(err)
	}
	c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	res, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Body, "finished") || res.AppStatus == 0 || time.Since(start) > 3*time.Second {
		t.Errorf("after abort: status %d, body %q, app status %d, %s", res.Status, res.Body, res.AppStatus, time.Since(start))
	}
	res, err = dialFCGI(t, addr).do(1, false, params(root, "GET", "/ok.php", nil), nil)
	if err != nil || res.Body != "ok" {
		t.Errorf("the next request: %v %+v", err, res)
	}
}
