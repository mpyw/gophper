package server_test

import (
	"os"
	"path/filepath"
	"testing"

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
