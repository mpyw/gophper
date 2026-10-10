//declscope:namespace engine

package gophper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
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

// TestNewEngineFails: a gophper-wasm of another ABI version, and host
// modules that cannot be set up, as when the OS refuses wazero memory for
// their code. A broken binary fails its first run.
func TestNewEngineFails(t *testing.T) {
	abi, wasi, host, cli := engineWASMABIVersion, engineInstantiateWASI, engineInstantiateHost, engineCLIBinary
	t.Cleanup(func() {
		engineWASMABIVersion, engineInstantiateWASI, engineInstantiateHost, engineCLIBinary = abi, wasi, host, cli
	})
	ctx := context.Background()
	engineWASMABIVersion = engineABIVersion + 1
	if _, err := NewEngine(ctx, EngineConfig{}); err == nil || !strings.Contains(err.Error(), "use matching versions") {
		t.Errorf("another ABI: %v", err)
	}
	engineWASMABIVersion = abi

	refused := errors.New("cannot allocate memory")
	engineInstantiateWASI = func(context.Context, wazero.Runtime) (api.Closer, error) { return nil, refused }
	if _, err := NewEngine(ctx, EngineConfig{}); !errors.Is(err, refused) {
		t.Errorf("WASI: %v", err)
	}
	engineInstantiateWASI = wasi
	engineInstantiateHost = func(wazero.HostModuleBuilder, context.Context) (api.Module, error) { return nil, refused }
	if _, err := NewEngine(ctx, EngineConfig{}); !errors.Is(err, refused) {
		t.Errorf("host modules: %v", err)
	}
	engineInstantiateHost = host

	broken := errors.New("gzip: invalid header")
	engineCLIBinary = func() ([]byte, error) { return nil, broken }
	e, err := NewEngine(ctx, EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(ctx) }()
	if _, err := e.RunCLI(ctx, Options{Args: []string{"-r", ""}}); !errors.Is(err, broken) || !strings.Contains(err.Error(), "decompress php.wasm") {
		t.Errorf("broken binary: %v", err)
	}
}
