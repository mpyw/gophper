package gophper_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper"
)

// TestProcess starts host programs from PHP. It needs a Unix shell.
func TestProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	for _, tt := range []struct {
		name string
		code string
		want string
	}{
		{"exec", `exec('echo a; echo b', $out, $code); echo implode(',', $out), " $code";`, "a,b 0"},
		{"exec exit code", `exec('echo partial; exit 4', $out, $code); echo implode(',', $out), " $code";`, "partial 4"},
		{"shell_exec", `echo trim(shell_exec('echo hi'));`, "hi"},
		{"system exit code", `ob_start(); system('exit 3', $rc); ob_end_clean(); echo $rc;`, "3"},
		{"passthru", `ob_start(); passthru('printf raw; exit 5', $rc); echo ob_get_clean(), " $rc";`, "raw 5"},
		{"proc_open pipes", `
			$p = proc_open(['cat'], [['pipe', 'r'], ['pipe', 'w']], $pipes);
			fwrite($pipes[0], 'through cat'); fclose($pipes[0]);
			echo stream_get_contents($pipes[1]), ' ', proc_close($p);`, "through cat 0"},
		{"proc_open env and cwd", `
			$p = proc_open(['sh', '-c', 'echo "$FOO $(pwd)"'], [1 => ['pipe', 'w']], $pipes, '/', ['FOO' => 'bar']);
			echo trim(stream_get_contents($pipes[1])); proc_close($p);`, "bar /"},
		{"proc_open socket", `
			$p = proc_open(['cat'], [['socket'], ['socket']], $pipes);
			fwrite($pipes[0], "line\n"); stream_socket_shutdown($pipes[0], STREAM_SHUT_WR);
			echo trim(fgets($pipes[1])); proc_close($p);`, "line"},
		{"proc_open pipes above stderr", `
			$p = proc_open(['sh', '-c', 'echo three >&3; echo five >&5'], [3 => ['pipe', 'w'], 5 => ['pipe', 'w']], $pipes);
			echo trim(stream_get_contents($pipes[3])), ' ', trim(stream_get_contents($pipes[5])), ' ', proc_close($p);`, "three five 0"},
		{"proc_open inherits stdout and stderr", `
			echo proc_close(proc_open(['echo', 'out'], [1 => STDOUT], $pipes)), "\n";
			echo proc_close(proc_open(['sh', '-c', 'echo err >&2'], [2 => STDERR], $pipes));`, "out\n0\nerr\n0"},
		{"proc_close waits", `
			$p = proc_open(['sh', '-c', 'sleep 0.1; exit 6'], [], $pipes);
			echo proc_close($p);`, "6"},
		{"proc_terminate", `
			$p = proc_open(['sleep', '10'], [], $pipes);
			proc_terminate($p);
			do { usleep(10000); $st = proc_get_status($p); } while ($st['running']);
			echo $st['termsig'];`, "15"},
		{"proc_terminate with SIGKILL", `
			$p = proc_open(['sleep', '10'], [], $pipes);
			var_export(proc_terminate($p, SIGKILL));
			do { usleep(10000); $st = proc_get_status($p); } while ($st['running']);
			echo ' ', $st['termsig'], ' ', var_export($st['signaled'], true);`, "true 9 true"},
		{"proc_terminate with an unknown signal", `
			$p = proc_open(['sleep', '10'], [], $pipes);
			var_export(proc_terminate($p, 99));
			proc_terminate($p, SIGKILL); proc_close($p);`, "false"},
		{"child killed by a signal", `
			$p = proc_open(['sh', '-c', 'kill -USR1 $$'], [], $pipes);
			do { usleep(10000); $st = proc_get_status($p); } while ($st['running']);
			echo $st['termsig'];`, "10"},
		{"child killed by a signal not in the table", `
			$p = proc_open(['sh', '-c', 'kill -SEGV $$'], [], $pipes);
			do { usleep(10000); $st = proc_get_status($p); } while ($st['running']);
			echo $st['termsig'];`, "11"},
		{"posix_kill reaches only children", `
			var_export([posix_kill(1, 0), posix_strerror(posix_get_last_error())]);`, "array (\n  0 => false,\n  1 => 'No such process',\n)"},
		{"posix_kill to a child", `
			$p = proc_open(['sleep', '10'], [], $pipes);
			$pid = proc_get_status($p)['pid'];
			var_export([posix_kill($pid, 0), posix_kill($pid, SIGTERM)]);
			// proc_close returns the raw status of a child killed by a signal.
			echo ' ', proc_close($p);`, "array (\n  0 => true,\n  1 => true,\n) 15"},
		{"pcntl_waitpid without children", `
			var_export([pcntl_waitpid(-1, $st, WNOHANG), pcntl_strerror(pcntl_get_last_error())]);`, "array (\n  0 => -1,\n  1 => 'No child process',\n)"},
		{"popen", `$h = popen('echo from popen; exit 2', 'r'); echo trim(fgets($h)), ' ', pclose($h);`, "from popen 2"},
		{"missing program", `var_dump(@proc_open(['/no/such/program'], [], $pipes));`, "bool(false)"},
		{"program not on PATH", `
			var_dump(@proc_open(['gophper-no-such-program'], [], $pipes)); echo error_get_last()['message'];`,
			"bool(false)\nproc_open(): posix_spawn() failed: No such file or directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, exit := runPHP(t, tt.code)
			if exit != 0 || strings.TrimSpace(out) != tt.want {
				t.Errorf("exit %d, got %q, want %q", exit, out, tt.want)
			}
		})
	}
}

// runProcessPHP runs code as runPHP does, with stdin, and with the host
// paths under the host path readOnly read-only to host functions. It returns the error of
// RunCLI.
func runProcessPHP(ctx context.Context, t *testing.T, code string, stdin io.Reader, readOnly string) (string, int, error) {
	t.Helper()
	var out bytes.Buffer
	exit, err := newTestEngine(t).RunCLI(ctx, gophper.Options{
		Args:   []string{"-r", code},
		Stdin:  stdin,
		Stdout: &out,
		Stderr: &out,
		FS:     gophper.HostFS(),
		HostPath: func(path string) (string, bool, bool) {
			host, writable, ok := gophper.HostPaths(path)
			return host, writable && (readOnly == "" || !strings.HasPrefix(host, readOnly)), ok
		},
		Processes: true,
	})
	return out.String(), exit, err
}

// TestProcessFiles gives children host files, the instance's stdin, and
// programs found by path.
func TestProcessFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	for name, body := range map[string]struct {
		text string
		mode os.FileMode
	}{
		"in":          {"from a file\n", 0o644},
		"ro/kept":     {"read-only\n", 0o644},
		"bin/greet":   {"#!/bin/sh\necho greet \"$@\"\n", 0o755},
		"here":        {"#!/bin/sh\necho here\n", 0o755},
		"plain":       {"#!/bin/sh\necho plain\n", 0o644},
		"bin/garbage": {"\x00\x01\x02\x03 not a program", 0o755},
	} {
		name = filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body.text), body.mode); err != nil {
			t.Fatal(err)
		}
	}
	// A host path, quoted as PHP sees it.
	php := func(s string) string { return fmt.Sprintf("%q", gophper.HostToGuest(s)) }
	for _, tt := range []struct {
		name  string
		code  string
		stdin io.Reader
		want  string
	}{
		{"stdin, stdout and append to files", fmt.Sprintf(`
			$out = %[1]s.'/out';
			proc_close(proc_open(['cat'], [['file', %[2]s, 'r'], ['file', $out, 'w']], $pipes));
			proc_close(proc_open(['echo', 'again'], [1 => ['file', $out, 'a']], $pipes));
			echo file_get_contents($out);`, php(dir), php(filepath.Join(dir, "in"))), nil, "from a file\nagain"},
		{"a file to read and write", fmt.Sprintf(`
			$f = %[1]s.'/rw'; file_put_contents($f, 'old');
			proc_close(proc_open(['sh', '-c', 'cat; echo new'], [['file', $f, 'r'], ['file', $f, 'w+']], $pipes));
			echo file_get_contents($f);`, php(dir)), nil, "new"},
		{"a read-only file to read", fmt.Sprintf(`
			$p = proc_open(['cat'], [['file', %s, 'r'], ['pipe', 'w']], $pipes);
			echo trim(stream_get_contents($pipes[1])), ' ', proc_close($p);`, php(filepath.Join(ro, "kept"))), nil, "read-only 0"},
		{"the instance's stdin", `
			$p = proc_open(['cat'], [STDIN, ['pipe', 'w']], $pipes);
			echo trim(stream_get_contents($pipes[1])), ' ', proc_close($p);`, strings.NewReader("typed in\n"), "typed in 0"},
		{"a relative PATH", fmt.Sprintf(`
			$p = proc_open(['greet', 'you'], [1 => ['pipe', 'w']], $pipes, %s, ['PATH' => 'bin']);
			echo trim(stream_get_contents($pipes[1])); proc_close($p);`, php(dir)), nil, "greet you"},
		{"an empty PATH entry is the cwd", fmt.Sprintf(`
			$p = proc_open(['here'], [1 => ['pipe', 'w']], $pipes, %s, ['PATH' => '/nonexistent:']);
			echo trim(stream_get_contents($pipes[1])); proc_close($p);`, php(dir)), nil, "here"},
		{"a relative path", fmt.Sprintf(`
			$p = proc_open(['./bin/greet'], [1 => ['pipe', 'w']], $pipes, %s);
			echo trim(stream_get_contents($pipes[1])); proc_close($p);`, php(dir)), nil, "greet"},
		{"a file that is not executable", fmt.Sprintf(`
			var_dump(@proc_open([%s], [], $pipes)); echo error_get_last()['message'];`, php(filepath.Join(dir, "plain"))),
			nil, "bool(false)\nproc_open(): posix_spawn() failed: Permission denied"},
		{"a file that is not a program", fmt.Sprintf(`
			var_dump(@proc_open([%s], [], $pipes)); echo error_get_last()['message'];`, php(filepath.Join(dir, "bin/garbage"))),
			nil, "bool(false)\nproc_open(): posix_spawn() failed: Exec format error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, exit, err := runProcessPHP(context.Background(), t, tt.code, tt.stdin, ro)
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || strings.TrimSpace(out) != tt.want {
				t.Errorf("exit %d, got %q, want %q", exit, out, tt.want)
			}
		})
	}
	if b, err := os.ReadFile(filepath.Join(ro, "kept")); err != nil || string(b) != "read-only\n" {
		t.Errorf("the read-only file changed: %q %v", b, err)
	}
}

// TestProcessWaitCanceled ends a run while proc_close waits for a child.
func TestProcessWaitCanceled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	ctx, ready, canceled := processCancelWhenReady(t)
	out, _, err := runProcessPHP(ctx, t, fmt.Sprintf(`
		$p = proc_open(['sleep', '3'], [], $pipes);
		echo "waiting\n";
		touch(%q);
		echo 'closed ', proc_close($p), "\n";`, gophper.HostToGuest(ready)), nil, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want the cancel", err)
	}
	if d := time.Since(canceled()); d > 2*time.Second {
		t.Errorf("the run took %v after the cancel", d)
	}
	// proc_close gives up once the run is over, rather than retrying. The
	// interrupts that stop the run may end the script before or after echo.
	if !strings.HasPrefix(out, "waiting\nclosed ") {
		t.Errorf("got %q", out)
	}
}

// processCancelWhenReady returns a context canceled 100ms after the script
// creates the file at ready, and when it was canceled. A deadline would
// count the time an instance takes to start, which a slow machine makes
// longer than the deadline.
func processCancelWhenReady(t *testing.T) (ctx context.Context, ready string, canceled func() time.Time) {
	t.Helper()
	ready = filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	at := make(chan time.Time, 1)
	go func() {
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				time.Sleep(100 * time.Millisecond)
				break
			}
		}
		at <- time.Now()
		cancel()
	}()
	return ctx, ready, func() time.Time { return <-at }
}

// TestProcessAbsolutePath starts a program by the path PHP sees, which on
// Windows is not the host's: /c/x/prog.exe is C:\x\prog.exe. The program is
// this test binary, which every OS can run.
func TestProcessAbsolutePath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, code := runPHP(t, fmt.Sprintf(`
		$p = proc_open([%q, "-test.run=^$"], [1 => ["pipe", "w"], 2 => ["pipe", "w"]], $pipes) or die("proc_open failed");
		$out = stream_get_contents($pipes[1]);
		echo proc_close($p), " ", str_contains($out, "PASS") ? "ran" : $out;
	`, gophper.HostToGuest(exe)))
	if code != 0 || out != "0 ran" {
		t.Errorf("exit %d: %q", code, out)
	}
}

// TestProcessStreamedStdin starts a child while the instance's stdin is a
// stream with nothing to read yet. The child gets a pipe, so waiting for it
// does not wait for the next input as well.
func TestProcessStreamedStdin(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }() // ends the feeder still reading
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
			Args: []string{"-r", fmt.Sprintf(`
				$p = proc_open([%q, "-test.run=^$"], [1 => ["pipe", "w"], 2 => ["pipe", "w"]], $pipes);
				stream_get_contents($pipes[1]);
				echo proc_close($p);`, gophper.HostToGuest(exe))},
			Stdin:     pr,
			Stdout:    &out,
			Stderr:    &out,
			Dir:       gophper.HostToGuest(wd),
			FS:        gophper.HostFS(),
			HostPath:  gophper.HostPaths,
			Processes: true,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil || out.String() != "0" {
			t.Errorf("%q, %v", out.String(), err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("proc_close waited for stdin")
	}
}
