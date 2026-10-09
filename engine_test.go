package gophper_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

func runCLI(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args:   args,
		Env:    []string{"TMPDIR=" + t.TempDir()},
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig().WithDirMount("/", "/"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func TestSmoke(t *testing.T) {
	script, err := filepath.Abs("testdata/smoke.php")
	if err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, "piped line\n", script)
	if code != 0 {
		t.Fatalf("exit code %d\n%s", code, out)
	}
	for _, want := range []string{
		"hooks: 100.0 C\n",
		"pipe: H 3\n",
		"generator: {\"1\":1,\"2\":4,\"3\":9}\n",
		"fiber: Exception: Fibers are not supported on this platform\n",
		"date: 2026-10-09T21:00:00+09:00\n",
		"password: ok\n",
		"file: written by wasm (16 bytes)\n",
		"stdin: piped line\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

// Fatal errors unwind through longjmp, which needs wasm exception handling.
func TestFatalErrorRunsShutdownFunctions(t *testing.T) {
	out, code := runCLI(t, "", "-r", `register_shutdown_function(fn () => print("shutdown\n")); undefined_fn();`)
	if code != 255 {
		t.Errorf("exit code = %d, want 255", code)
	}
	if !strings.Contains(out, "Call to undefined function undefined_fn()") || !strings.HasSuffix(out, "shutdown\n") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestDir(t *testing.T) {
	dir, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args:   []string{"-r", `echo getcwd(), "|", file_exists("smoke.php") ? "found" : "missing", "|", getenv("GOPHPER_CWD") === false ? "hidden" : "visible";`},
		Dir:    dir,
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig().WithDirMount("/", "/"),
	})
	if err != nil || code != 0 {
		t.Fatal(code, err, out.String())
	}
	if want := dir + "|found|hidden"; out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestExitCode(t *testing.T) {
	if _, code := runCLI(t, "", "-r", `function a($n) { $n === 0 ? exit(7) : a($n - 1); } a(100);`); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

func TestTimeout(t *testing.T) {
	// Timed from inside PHP: starting an instance takes time of its own, and
	// more on a slow machine.
	const loop = `$t = hrtime(true); register_shutdown_function(function () use ($t) { printf("shutdown after %d ms\n", (hrtime(true) - $t) / 1e6); }); for (;;) {}`
	for name, args := range map[string][]string{
		"max_execution_time": {"-d", "max_execution_time=1", "-r", loop},
		"set_time_limit":     {"-r", "set_time_limit(1); " + loop},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			code, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{Args: args, Stdout: &out, Stderr: &out})
			if err != nil {
				t.Fatal(err)
			}
			m := regexp.MustCompile(`shutdown after (\d+) ms\n$`).FindStringSubmatch(out.String())
			if code != 255 || !strings.Contains(out.String(), "Maximum execution time of 1 second exceeded") || m == nil {
				t.Fatalf("exit code %d\n%s", code, out.String())
			}
			if ms, _ := strconv.Atoi(m[1]); ms < 950 || ms > 2000 {
				t.Errorf("stopped after %d ms, want about 1000", ms)
			}
		})
	}
}

// A timeout interrupts only the sleep in progress, as a signal would.
// The shutdown function sleeps again and must sleep in full.
func TestTimeoutDuringSleep(t *testing.T) {
	var out bytes.Buffer
	code, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args: []string{"-r", `register_shutdown_function(function () {
			$t = hrtime(true); usleep(200000); printf("slept %d ms\n", (hrtime(true) - $t) / 1e6);
		}); set_time_limit(1); sleep(10);`},
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 255 || !regexp.MustCompile(`slept (19\d|2\d\d) ms\n$`).MatchString(out.String()) {
		t.Errorf("exit code %d\n%s", code, out.String())
	}
}

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := newTestEngine(t).RunCLI(ctx, gophper.Options{Args: []string{"-r", `for (;;) {}`}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// A cancel that arrives while PHP is still starting must not be lost:
// php_request_startup() clears the interrupt flags.
func TestCancelDuringStartup(t *testing.T) {
	e := newTestEngine(t)
	for _, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), delay)
		start := time.Now()
		_, err := e.RunCLI(ctx, gophper.Options{Args: []string{"-r", `for (;;) {}`}})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("delay %s: err = %v", delay, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("delay %s: took %s", delay, d)
		}
	}
}

// newEngineWith returns an Engine with cfg, closed when the test ends.
func newEngineWith(t *testing.T, cfg gophper.EngineConfig) *gophper.Engine {
	t.Helper()
	e, err := gophper.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// runEngineCLI runs code with e and returns the output.
func runEngineCLI(t *testing.T, e *gophper.Engine, code string) string {
	t.Helper()
	var out bytes.Buffer
	exit, err := e.RunCLI(context.Background(), gophper.Options{Args: []string{"-r", code}, Stdout: &out, Stderr: &out})
	if err != nil || exit != 0 {
		t.Fatalf("exit %d, err %v\n%s", exit, err, out.String())
	}
	return out.String()
}

// The cache directory keeps the decompressed binary, which the next
// Engine reads instead of decompressing again.
func TestEngineCacheDir(t *testing.T) {
	dir := t.TempDir()
	// Share wazero's compiled code with the default cache, so that only the
	// binaries start empty.
	if def := gophper.DefaultEngineConfig().CacheDir; def != "" {
		compiled, _ := filepath.Glob(filepath.Join(def, "wazero-*"))
		for _, c := range compiled {
			if err := os.Symlink(c, filepath.Join(dir, filepath.Base(c))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := runEngineCLI(t, newEngineWith(t, gophper.EngineConfig{CacheDir: dir}), `echo "first";`); got != "first" {
		t.Fatalf("got %q", got)
	}
	cached, err := filepath.Glob(filepath.Join(dir, "wasm", "*.wasm"))
	if err != nil || len(cached) != 1 {
		t.Fatalf("cached binaries: %v, %v", cached, err)
	}
	info, err := os.Stat(cached[0])
	if err != nil || info.Size() < 1<<20 {
		t.Fatalf("cached binary: %v, %v", info, err)
	}
	if got := runEngineCLI(t, newEngineWith(t, gophper.EngineConfig{CacheDir: dir}), `echo "second";`); got != "second" {
		t.Errorf("from the cache: %q", got)
	}

	// An empty file, as a crash could leave, is replaced.
	if err := os.WriteFile(cached[0], nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runEngineCLI(t, newEngineWith(t, gophper.EngineConfig{CacheDir: dir}), `echo "third";`); got != "third" {
		t.Errorf("over an empty file: %q", got)
	}
	if info, err := os.Stat(cached[0]); err != nil || info.Size() < 1<<20 {
		t.Errorf("the empty file was not replaced: %v, %v", info, err)
	}
	if leftover, _ := filepath.Glob(filepath.Join(dir, "wasm", ".wasm-*")); len(leftover) != 0 {
		t.Errorf("temporary files left: %v", leftover)
	}

	// Failing to keep the binary only costs time. Here "wasm" is taken by a file.
	if err := os.RemoveAll(filepath.Join(dir, "wasm")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wasm"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runEngineCLI(t, newEngineWith(t, gophper.EngineConfig{CacheDir: dir}), `echo "fourth";`); got != "fourth" {
		t.Errorf("without a place for the binary: %q", got)
	}
}

func TestEngineCacheDirInvalid(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := gophper.NewEngine(context.Background(), gophper.EngineConfig{CacheDir: file})
	if err == nil {
		_ = e.Close(context.Background())
		t.Fatal("no error for a cache directory that is a file")
	}
	if !strings.Contains(err.Error(), "compilation cache") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaultEngineConfigWithoutCacheDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", "")
	if cfg := gophper.DefaultEngineConfig(); cfg != (gophper.EngineConfig{}) {
		t.Errorf("got %+v, want no cache directory", cfg)
	}
}

func TestEngineBuildID(t *testing.T) {
	a := newEngineWith(t, gophper.EngineConfig{}).BuildID()
	b := newEngineWith(t, gophper.EngineConfig{}).BuildID()
	if a == "" || a != b {
		t.Errorf("build ids %q and %q", a, b)
	}
}

// PHP_BINARY reports EngineConfig.PHPBinary, and PHP can see it.
func TestEnginePHPBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "php")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := gophper.DefaultEngineConfig()
	cfg.PHPBinary = bin
	got := runEngineCLI(t, newEngineWith(t, cfg), `echo PHP_BINARY, "|", is_file(PHP_BINARY) ? "visible" : "hidden";`)
	if want := filepath.ToSlash(bin) + "|visible"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := runEngineCLI(t, newTestEngine(t), `var_dump(PHP_BINARY);`); got != "string(0) \"\"\n" {
		t.Errorf("without PHPBinary: %q", got)
	}
}

// RunCGI runs php-cgi once: the request comes from the environment, and
// the response has CGI headers.
func TestEngineRunCGI(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "index.php")
	if err := os.WriteFile(script, []byte(`<?php header("X-Test: yes"); echo $_SERVER["REQUEST_METHOD"], " ", $_GET["q"], " ", file_get_contents("php://input");`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code, err := newTestEngine(t).RunCGI(context.Background(), gophper.Options{
		Env: []string{
			"REDIRECT_STATUS=200", "GATEWAY_INTERFACE=CGI/1.1", "REQUEST_METHOD=POST", "QUERY_STRING=q=1",
			"SCRIPT_FILENAME=" + script, "CONTENT_LENGTH=4", "CONTENT_TYPE=text/plain",
		},
		Stdin:  strings.NewReader("body"),
		Stdout: &out,
		Stderr: &stderr,
		FS:     wazero.NewFSConfig().WithReadOnlyDirMount(dir, dir).WithDirMount(t.TempDir(), "/tmp"),
	})
	if err != nil || code != 0 {
		t.Fatalf("exit %d, err %v\n%s", code, err, stderr.String())
	}
	head, body, _ := strings.Cut(out.String(), "\r\n\r\n")
	if !strings.Contains(head, "X-Test: yes") || body != "POST 1 body" {
		t.Errorf("got %q", out.String())
	}
}

// An ExtensionDir that does not exist leaves PHP without extensions.
func TestEngineMissingExtensionDir(t *testing.T) {
	cfg := gophper.DefaultEngineConfig()
	cfg.ExtensionDir = filepath.Join(t.TempDir(), "missing")
	var out bytes.Buffer
	code, err := newEngineWith(t, cfg).RunCLI(context.Background(), gophper.Options{
		Args:   []string{"-d", "extension=dl_test", "-r", `var_dump(extension_loaded("dl_test"));`},
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil || code != 0 || !strings.Contains(out.String(), "Unable to load dynamic library 'dl_test'") || !strings.HasSuffix(out.String(), "bool(false)\n") {
		t.Errorf("exit %d, err %v\n%s", code, err, out.String())
	}
}

// TestEngineCloseDuringRun closes the engine under a running script. Close
// stops the run and waits for it, and a later run is refused.
func TestEngineCloseDuringRun(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		_, err := engine.RunCLI(context.Background(), gophper.Options{
			Args:   []string{"-r", `echo "started\n"; for (;;) {}`},
			Stdout: &engineFirstWrite{f: func() { close(started) }},
		})
		ended <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the script did not start")
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("Close returned before the run ended")
	}
	if _, err := engine.RunCLI(context.Background(), gophper.Options{Args: []string{"-r", "echo 1;"}}); !errors.Is(err, gophper.ErrEngineClosed) {
		t.Errorf("a run after Close: %v, want ErrEngineClosed", err)
	}
}

// engineFirstWrite calls f on the first write.
type engineFirstWrite struct {
	once sync.Once
	f    func()
}

func (w *engineFirstWrite) Write(p []byte) (int, error) {
	w.once.Do(w.f)
	return len(p), nil
}
