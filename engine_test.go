package gophper_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
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
	const loop = `register_shutdown_function(fn () => print("shutdown\n")); for (;;) {}`
	for name, args := range map[string][]string{
		"max_execution_time": {"-d", "max_execution_time=1", "-r", loop},
		"set_time_limit":     {"-r", "set_time_limit(1); " + loop},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			start := time.Now()
			code, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{Args: args, Stdout: &out, Stderr: &out})
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < time.Second || d > 2500*time.Millisecond {
				t.Errorf("took %s, want about 1s", d)
			}
			if code != 255 || !strings.Contains(out.String(), "Maximum execution time of 1 second exceeded") || !strings.HasSuffix(out.String(), "shutdown\n") {
				t.Errorf("exit code %d\n%s", code, out.String())
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
