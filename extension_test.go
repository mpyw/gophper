package gophper_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phpext "github.com/mpyw/gophper-wasm/ext"

	"github.com/mpyw/gophper"
)

// newExtensionEngine returns an Engine whose ExtensionDir holds every
// extension that comes with gophper-wasm, such as dl_test, php-src's
// extension for testing dl().
func newExtensionEngine(t *testing.T) *gophper.Engine {
	t.Helper()
	dir := t.TempDir()
	for _, name := range phpext.Names() {
		so, err := phpext.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".so"), so, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := gophper.DefaultEngineConfig()
	cfg.ExtensionDir = dir
	e, err := gophper.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(context.Background()) })
	return e
}

func runExtension(t *testing.T, e *gophper.Engine, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code, err := e.RunCLI(context.Background(), gophper.Options{Args: args, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func TestExtensionFromINI(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-d", "extension=dl_test", "-r", `
		var_dump(extension_loaded("dl_test"), dl_test_test2("gophper"), ini_get("dl_test.long"));
		echo (new DlTest())->test("method"), "\n";
		try { dl_test_test2([]); } catch (TypeError $e) { echo get_class($e), "\n"; }
	`)
	want := "bool(true)\nstring(13) \"Hello gophper\"\nstring(1) \"0\"\nHello method\nTypeError\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestExtensionFromDL(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-r", `var_dump(extension_loaded("dl_test"), dl("dl_test.so"), dl_test_test2("dl"));`)
	if code != 0 || out != "bool(false)\nbool(true)\nstring(8) \"Hello dl\"\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A fatal error while the extension's code is on the stack unwinds through
// it, and shutdown functions still run.
func TestExtensionFatalError(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-d", "extension=dl_test", "-r", `
		register_shutdown_function(fn () => print("shutdown ran\n"));
		class Boom { function __toString(): string { undefined_fn(); return ""; } }
		dl_test_test2(new Boom);
	`)
	if code != 255 || !strings.Contains(out, "Call to undefined function undefined_fn()") || !strings.HasSuffix(out, "shutdown ran\n") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestExtensionMissing(t *testing.T) {
	out, _ := runExtension(t, newExtensionEngine(t), "-d", "extension=nope", "-r", `echo "still runs\n";`)
	if !strings.Contains(out, `Unable to load dynamic library 'nope'`) || !strings.Contains(out, "still runs") {
		t.Errorf("got\n%s", out)
	}
}

func TestExtensionNotSharedLibrary(t *testing.T) {
	dir := t.TempDir()
	// A valid but empty module: no dylink.0 section.
	if err := os.WriteFile(filepath.Join(dir, "plain.so"), []byte("\x00asm\x01\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := gophper.DefaultEngineConfig()
	cfg.ExtensionDir = dir
	e, err := gophper.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	out, _ := runExtension(t, e, "-d", "extension=plain", "-r", `echo "still runs\n";`)
	if !strings.Contains(out, "not a shared library") || !strings.Contains(out, "still runs") {
		t.Errorf("got\n%s", out)
	}
}

// TestExtensionBundled loads every extension that comes with gophper-wasm.
func TestExtensionBundled(t *testing.T) {
	for _, name := range phpext.Names() {
		t.Run(name, func(t *testing.T) {
			out, code := runExtension(t, newExtensionEngine(t), "-d", "extension="+name, "-r", `echo extension_loaded("`+name+`") ? "loaded" : "missing";`)
			if code != 0 || out != "loaded" {
				t.Errorf("exit %d\n%s", code, out)
			}
		})
	}
}
