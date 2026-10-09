package server_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mpyw/gophper/server"
)

// TestMergeINIFile checks that a section in the file does not capture the
// settings after it.
func TestMergeINIFile(t *testing.T) {
	for _, section := range []string{"[PATH=/nowhere]", `[ "path=/nowhere"]`, "['HOST=nowhere']"} {
		testMergeINIFile(t, section)
	}
}

func testMergeINIFile(t *testing.T, section string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(file, []byte("memory_limit=77M\n"+section+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ini, err := server.MergeINIFile(file, []string{"memory_limit=99M"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.php"), []byte(`<?php echo ini_get("memory_limit");`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := startHTTPWith(t, func(c *server.HTTPConfig) {
		c.Root = dir
		c.Mounts = []server.Mount{{Dir: dir}}
		c.INI = ini
	})
	if _, body := get(t, srv.URL+"/"); body != "99M" {
		t.Errorf("%s: memory_limit = %q, want 99M", section, body)
	}
}
