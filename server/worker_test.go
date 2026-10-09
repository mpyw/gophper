package server_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mpyw/gophper/server"
)

// TestHTTPWorkers sends more requests than a worker serves: each must
// still succeed, on a fresh worker, and see none of the last one's state.
func TestHTTPWorkers(t *testing.T) {
	for _, noWorkers := range []bool{false, true} {
		root := t.TempDir()
		script := `<?php $GLOBALS["n"] = ($GLOBALS["n"] ?? 0) + 1; echo $GLOBALS["n"];`
		if err := os.WriteFile(filepath.Join(root, "index.php"), []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := startHTTPWith(t, func(c *server.HTTPConfig) {
			c.Root = root
			c.Mounts = []server.Mount{{Dir: root}}
			c.Concurrency = 1
			c.MaxRequests = 2
			c.NoWorkers = noWorkers
		})
		for i := range 5 {
			res, body := get(t, srv.URL+"/")
			if res.StatusCode != 200 || body != "1" {
				t.Errorf("no workers %v, request %d: %d %q", noWorkers, i, res.StatusCode, body)
			}
		}
	}
}

// TestHTTPWorkersOpcache checks where opcache keeps scripts: a worker in
// its shared memory, from one request to the next, and a fresh instance
// in the file cache only.
func TestHTTPWorkersOpcache(t *testing.T) {
	for _, noWorkers := range []bool{false, true} {
		root := t.TempDir()
		script := `<?php $s = opcache_get_status(false);
echo json_encode([$s["file_cache_only"] ?? false, ($s["opcache_statistics"]["hits"] ?? 0) > 0]);`
		file := filepath.Join(root, "index.php")
		if err := os.WriteFile(file, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		// opcache leaves a file alone for 2 seconds after it changes
		// (opcache.file_update_protection).
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
		srv := startHTTPWith(t, func(c *server.HTTPConfig) {
			c.Root = root
			c.Mounts = []server.Mount{{Dir: root}}
			c.Concurrency = 1
			c.NoWorkers = noWorkers
			c.OpcacheDir = t.TempDir()
		})
		get(t, srv.URL+"/")
		_, body := get(t, srv.URL+"/")
		want := `[false,true]`
		if noWorkers {
			want = `[true,false]`
		}
		if body != want {
			t.Errorf("no workers %v: [file_cache_only, shared memory hit] = %s, want %s", noWorkers, body, want)
		}
	}
}

// TestHTTPSlowBody stalls in the middle of a body. The server waits for
// it without holding the only PHP instance, so other requests still run.
func TestHTTPSlowBody(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.php"), []byte(`<?php echo strlen(file_get_contents("php://input"));`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := startHTTPWith(t, func(c *server.HTTPConfig) {
		c.Root = root
		c.Mounts = []server.Mount{{Dir: root}}
		c.Concurrency = 1
		c.MaxWaitTime = 5 * time.Second
	})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Larger than what is kept in memory, so that it goes to a file.
	const size = 3 << 20
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: "+strconv.Itoa(size)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(make([]byte, size/2)); err != nil {
		t.Fatal(err)
	}

	if res, body := get(t, srv.URL+"/"); res.StatusCode != 200 || body != "0" {
		t.Errorf("another request while a body stalls: %d %q", res.StatusCode, body)
	}

	if _, err := conn.Write(make([]byte, size-size/2)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != strconv.Itoa(size) {
		t.Errorf("the slow request: %d %q", res.StatusCode, b)
	}
}

// TestHTTPNoOpcache checks that NoOpcache turns opcache off, in workers and
// in fresh instances. php-cgi turns it on by default.
func TestHTTPNoOpcache(t *testing.T) {
	for _, noWorkers := range []bool{false, true} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "index.php"), []byte(`<?php var_export(opcache_get_status(false));`), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := startHTTPWith(t, func(c *server.HTTPConfig) {
			c.Root = root
			c.Mounts = []server.Mount{{Dir: root}}
			c.NoWorkers = noWorkers
			c.NoOpcache = true
		})
		if _, body := get(t, srv.URL+"/"); body != "false" {
			t.Errorf("no workers %v: opcache_get_status() = %s", noWorkers, body)
		}
	}
}
