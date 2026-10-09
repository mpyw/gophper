package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

func startHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHTTPHandler(engine, server.HTTPConfig{Root: "testdata/app", TempDir: t.TempDir(), Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		h.Close()
		engine.Close(context.Background())
	})
	return srv
}

// noRedirect returns the first response, without following Location.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := noRedirect.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
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
	res.Body.Close()
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
	res.Body.Close()
	if !strings.Contains(string(body), `"raw":"chunked body"`) {
		t.Errorf("got %s", body)
	}
}

func TestHTTPS(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	h, err := server.NewHTTPHandler(engine, server.HTTPConfig{Root: "testdata/app", TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
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
	defer res.Body.Close()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("%q %v", line, err)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Errorf("first line took %s; output was buffered", d)
	}
}
