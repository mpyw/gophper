package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

func startHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	return startHTTPWith(t, func(*server.HTTPConfig) {})
}

//declscope:shared // worker_test.go
func startHTTPWith(t *testing.T, configure func(*server.HTTPConfig)) *httptest.Server {
	t.Helper()
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.HTTPConfig{Root: "testdata/app", PHPConfig: server.PHPConfig{TempDir: t.TempDir(), Concurrency: 4}}
	configure(&cfg)
	h, err := server.NewHTTPHandler(engine, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		_ = h.Close()
		_ = engine.Close(context.Background())
	})
	return srv
}

// writerFunc adapts a function to io.Writer.
//
//declscope:shared // fastcgi_test.go collects logs with it too
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// noRedirect returns the first response, without following Location.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

//declscope:shared // worker_test.go
func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := noRedirect.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestHTTPRouting(t *testing.T) {
	srv := startHTTP(t)
	for _, tc := range []struct {
		path, status, body string
	}{
		{"/assets/style.css", "200", "body { color: red; }\n"},
		{"/sub/page.php/extra/info", "200", "page.php, path info /extra/info\n"},
		{"/sub/page.php", "200", "page.php, path info (none)\n"},
		{"/sub/", "200", "sub index, script /sub/index.php\n"},
		{"/sub", "301", ""},
		{"/created", "201", "created\n"},
		{"/redirect", "302", ""},
	} {
		res, body := get(t, srv.URL+tc.path)
		if got := res.Status[:3]; got != tc.status || (tc.body != "" && body != tc.body) {
			t.Errorf("%s: status %s, body %q", tc.path, got, body)
		}
		if tc.path == "/sub" && res.Header.Get("Location") != "/sub/" {
			t.Errorf("/sub: Location %q", res.Header.Get("Location"))
		}
		if tc.path == "/redirect" && res.Header.Get("Location") != "/target" {
			t.Errorf("/redirect: Location %q", res.Header.Get("Location"))
		}
		if tc.path == "/created" && res.Header.Get("X-Custom") != "yes" {
			t.Errorf("/created: X-Custom %q", res.Header.Get("X-Custom"))
		}
	}
	// PHP source is never sent as a file.
	if _, body := get(t, srv.URL+"/sub/page.php"); strings.Contains(body, "<?php") {
		t.Errorf("PHP source leaked: %q", body)
	}
}

func TestHTTPFrontController(t *testing.T) {
	srv := startHTTP(t)
	res, body := get(t, srv.URL+"/users/42?tab=posts")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q\n%s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	want := map[string]any{
		"script_name":     "/index.php",
		"script_filename": "index.php",
		"request_uri":     "/users/42?tab=posts",
		"https":           "",
		"host":            strings.TrimPrefix(srv.URL, "http://"),
		"software":        "gophper",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if q, _ := got["query"].(map[string]any); q["tab"] != "posts" {
		t.Errorf("query = %v", got["query"])
	}
}

func TestHTTPPost(t *testing.T) {
	srv := startHTTP(t)
	req, _ := http.NewRequest("POST", srv.URL+"/post", strings.NewReader(url.Values{"name": {"gophper"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "a=1; b=2")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(body) != `{"post":{"name":"gophper"},"cookie":{"a":"1","b":"2"},"raw":"name=gophper"}` {
		t.Errorf("got %s", body)
	}

	// A chunked body has no Content-Length, and still reaches PHP.
	req, _ = http.NewRequest("POST", srv.URL+"/post", io.MultiReader(strings.NewReader("chunked "), strings.NewReader("body")))
	req.ContentLength = -1
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(body), `"raw":"chunked body"`) {
		t.Errorf("got %s", body)
	}
}

func TestHTTPS(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	h, err := server.NewHTTPHandler(engine, server.HTTPConfig{Root: "testdata/app", PHPConfig: server.PHPConfig{TempDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(body), `"https":"on"`) {
		t.Errorf("got %s", body)
	}
}

// flush() in PHP reaches the client before the script ends.
func TestHTTPStreaming(t *testing.T) {
	srv := startHTTP(t)
	start := time.Now()
	res, err := http.Get(srv.URL + "/stream.php")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("%q %v", line, err)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Errorf("first line took %s; output was buffered", d)
	}
}

// projectConfig is a Laravel-shaped layout: the document root is public/,
// and PHP may reach the whole project.
func projectConfig(cfg *server.HTTPConfig) {
	project, _ := filepath.Abs("testdata/project")
	cfg.Root = filepath.Join(project, "public")
	cfg.Mounts = []server.Mount{{Dir: project, ReadOnly: true}}
}

func TestHTTPProjectLayout(t *testing.T) {
	srv := startHTTPWith(t, projectConfig)
	for path, want := range map[string]string{
		"/users/1":                  "hello from vendor at /users/1\n",
		"/robots.txt":               "static\n",
		"/.well-known/security.txt": "ok\n",
	} {
		res, body := get(t, srv.URL+path)
		if res.StatusCode != 200 || body != want {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
	}
	// Dotfiles are never served, as Laravel's nginx config denies them.
	for _, path := range []string{"/.env", "/.hidden/file.txt", "/.git/config"} {
		if res, body := get(t, srv.URL+path); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
	}
}

func TestHTTPRootOutsideMounts(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	other := t.TempDir()
	_, err = server.NewHTTPHandler(engine, server.HTTPConfig{Root: "testdata/app", PHPConfig: server.PHPConfig{Mounts: []server.Mount{{Dir: other}}}})
	if err == nil || !strings.Contains(err.Error(), "outside every mount") {
		t.Errorf("err = %v", err)
	}
}

func TestHTTPRouter(t *testing.T) {
	srv := startHTTPWith(t, func(cfg *server.HTTPConfig) {
		projectConfig(cfg)
		cfg.Router = "../router.php"
	})
	res, body := get(t, srv.URL+"/anything?x=1")
	// As with php -S, $_SERVER describes the front controller. The router
	// runs from the document root, since PHP cannot reach the directory
	// the test started in.
	want := "router: /anything?x=1 script /index.php path info /anything cwd public\nrouter env leaked: false\n"
	if res.StatusCode != 200 || body != want {
		t.Errorf("%d %q", res.StatusCode, body)
	}
	// return false sends the file as is, and a missing one is 404.
	if res, body := get(t, srv.URL+"/robots.txt"); res.StatusCode != 200 || body != "static\n" {
		t.Errorf("robots.txt: %d %q", res.StatusCode, body)
	}
	if res, _ := get(t, srv.URL+"/missing.txt"); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing.txt: %d", res.StatusCode)
	}
}

func TestHTTPRoutingOptions(t *testing.T) {
	srv := startHTTPWith(t, func(cfg *server.HTTPConfig) {
		cfg.NoStatic = true
		cfg.NoFrontController = true
		cfg.Index = []string{"page.php", "index.php"}
	})
	for path, want := range map[string]int{
		"/assets/style.css": http.StatusNotFound, // NoStatic
		"/users/1":          http.StatusNotFound, // NoFrontController
		"/sub/":             http.StatusOK,       // page.php comes first in Index
	} {
		res, body := get(t, srv.URL+path)
		if res.StatusCode != want {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
		if path == "/sub/" && !strings.HasPrefix(body, "page.php") {
			t.Errorf("/sub/ ran %q", body)
		}
	}
}

func TestHTTPMaxBodySize(t *testing.T) {
	srv := startHTTPWith(t, func(cfg *server.HTTPConfig) { cfg.MaxBodySize = 10 })
	for _, chunked := range []bool{false, true} {
		req, _ := http.NewRequest("POST", srv.URL+"/post", strings.NewReader(strings.Repeat("x", 100)))
		if chunked {
			req.ContentLength = -1
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("chunked=%v: status %d", chunked, res.StatusCode)
		}
	}
}

// With every instance busy past MaxWaitTime, a request fails fast with 503.
func TestHTTPMaxWaitTime(t *testing.T) {
	srv := startHTTPWith(t, func(cfg *server.HTTPConfig) {
		cfg.Concurrency = 1
		cfg.MaxWaitTime = 100 * time.Millisecond
	})
	slow := make(chan int)
	go func() {
		res, err := http.Get(srv.URL + "/slow.php?ms=800")
		if err != nil {
			slow <- 0
			return
		}
		_ = res.Body.Close()
		slow <- res.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	res, _ := get(t, srv.URL+"/slow.php?ms=1")
	if res.StatusCode != http.StatusServiceUnavailable || time.Since(start) > 500*time.Millisecond {
		t.Errorf("status %d after %s", res.StatusCode, time.Since(start))
	}
	if code := <-slow; code != 200 {
		t.Errorf("slow request: %d", code)
	}
}

func TestHTTPAccessLog(t *testing.T) {
	var log strings.Builder
	var mu sync.Mutex
	srv := startHTTPWith(t, func(cfg *server.HTTPConfig) {
		cfg.AccessLog = writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return log.Write(p) })
	})
	get(t, srv.URL+"/created")
	mu.Lock()
	defer mu.Unlock()
	if !regexp.MustCompile(`^127\.0\.0\.1 - - \[[^]]+\] "GET /created HTTP/1.1" 201 8 [0-9.]+s\n$`).MatchString(log.String()) {
		t.Errorf("access log: %q", log.String())
	}
}
