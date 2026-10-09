//go:build unix

package server_test

import (
	"net/http"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mpyw/gophper/server"
)

// TestHTTPScriptNameNotAFile asks for x.php that is a named pipe, which
// is no regular file: splitScript does not run it, and it is not sent as a
// file either, which would also block on the pipe.
func TestHTTPScriptNameNotAFile(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.php"), 0o600); err != nil {
		t.Skip(err)
	}
	srv := startHTTPWith(t, func(cfg *server.HTTPConfig) {
		cfg.Root = root
		cfg.NoFrontController = true
	})
	if res, body := get(t, srv.URL+"/pipe.php"); res.StatusCode != http.StatusNotFound {
		t.Errorf("%d %q", res.StatusCode, body)
	}
}
