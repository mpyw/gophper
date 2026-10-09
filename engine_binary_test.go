//declscope:namespace engine

package gophper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestEngineBinary decompresses each time without a cache directory, and
// once with one. A failed decompression is reported, and a cache that
// cannot be written only costs the next run.
func TestEngineBinary(t *testing.T) {
	calls := 0
	bin := func() ([]byte, error) { calls++; return []byte("wasm"), nil }
	for range 2 {
		if b, err := engineBinary("", bin, "d"); err != nil || string(b) != "wasm" {
			t.Fatalf("%q, %v", b, err)
		}
	}
	if calls != 2 {
		t.Errorf("without a cache: %d calls, want 2", calls)
	}

	dir := t.TempDir()
	calls = 0
	for range 2 {
		if b, err := engineBinary(dir, bin, "d"); err != nil || string(b) != "wasm" {
			t.Fatalf("%q, %v", b, err)
		}
	}
	if calls != 1 {
		t.Errorf("with a cache: %d calls, want 1", calls)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "wasm", "d.wasm")); err != nil || string(b) != "wasm" {
		t.Errorf("cached %q, %v", b, err)
	}

	broken := errors.New("broken")
	if _, err := engineBinary(dir, func() ([]byte, error) { return nil, broken }, "other"); !errors.Is(err, broken) {
		t.Errorf("failed decompression: %v", err)
	}

	// A file where the cache directory would go.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := engineBinary(file, bin, "d"); err != nil || string(b) != "wasm" {
		t.Errorf("unwritable cache: %q, %v", b, err)
	}
}
