package gophper_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

func TestSystemHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix permissions and locks")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		code string
		want string
	}{
		{"uid", `echo posix_getuid();`, fmt.Sprint(os.Getuid())},
		{"user", `echo posix_getpwuid(posix_getuid()) !== false ? 'found' : 'missing';`, "found"},
		// wasi-libc's utsname holds 64 bytes of it, so php_uname() may cut a
		// longer one short, as macOS CI runners' names are.
		{"hostname", `echo gethostname() !== '' && substr(gethostname(), 0, 64) === php_uname('n') ? 'same' : 'differ';`, "same"},
		{"fileperms", fmt.Sprintf(`printf('%%o', fileperms(%q) & 0777);`, file), "750"},
		{"fileowner", fmt.Sprintf(`echo fileowner(%q);`, file), fmt.Sprint(os.Getuid())},
		{"chmod", fmt.Sprintf(`chmod(%[1]q, 0640); clearstatcache(); printf('%%o', fileperms(%[1]q) & 0777);`, file), "640"},
		{"getpwnam", `$u = posix_getpwuid(posix_getuid()); echo posix_getpwnam($u['name'])['uid'] === posix_getuid() ? 'same' : 'differ';`, "same"},
		{"missing user", `var_export([posix_getpwnam('gophper-no-such-user'), posix_getpwuid(987654)]);`, "array (\n  0 => false,\n  1 => false,\n)"},
		{"group", `$g = posix_getgrgid(posix_getgid()); echo $g !== false && posix_getgrnam($g['name'])['gid'] === posix_getgid() ? 'same' : 'differ';`, "same"},
		{"missing group", `var_export([posix_getgrnam('gophper-no-such-group'), posix_getgrgid(987654)]);`, "array (\n  0 => false,\n  1 => false,\n)"},
		{"chown to self", fmt.Sprintf(`var_export([chown(%[1]q, posix_getuid()), chgrp(%[1]q, posix_getgid())]);`, file), "array (\n  0 => true,\n  1 => true,\n)"},
		{"chown missing", fmt.Sprintf(`var_export(@chown(%q, posix_getuid())); echo ' ', error_get_last()['message'];`, file+".missing"), "false chown(): No such file or directory"},
		{"chmod special bits", fmt.Sprintf(`
			mkdir(%[1]q); chmod(%[1]q, 01777); chmod(%[2]q, 04750); clearstatcache();
			printf('%%o %%o', fileperms(%[1]q) & 07777, fileperms(%[2]q) & 07777); chmod(%[2]q, 0750);`, filepath.Join(dir, "sticky"), file), "1777 4750"},
		{"flock", fmt.Sprintf(`
			$a = fopen(%[1]q, 'r'); $b = fopen(%[1]q, 'r');
			var_export([flock($a, LOCK_EX | LOCK_NB), flock($b, LOCK_EX | LOCK_NB), flock($a, LOCK_UN), flock($b, LOCK_EX | LOCK_NB)]);`, file),
			"array (\n  0 => true,\n  1 => false,\n  2 => true,\n  3 => true,\n)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, exit := runPHP(t, tt.code)
			if exit != 0 || strings.TrimSpace(out) != tt.want {
				t.Errorf("exit %d, got %q, want %q", exit, out, tt.want)
			}
		})
	}
}

// TestSystemSandbox runs PHP without HostPath and Processes: it must not
// reach host programs or change host files.
func TestSystemSandbox(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := fmt.Sprintf(`var_export([@exec('echo escaped'), @proc_open(['echo'], [], $p), @chmod(%q, 0777)]);`, file)
	exit, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args:   []string{"-r", code},
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig().WithDirMount(dir, dir),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "array (\n  0 => false,\n  1 => false,\n  2 => false,\n)"; exit != 0 || out.String() != want {
		t.Errorf("exit %d, got %q, want %q", exit, out.String(), want)
	}
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the host file changed: %v %v", st.Mode(), err)
	}
}

// runSystemPHP runs code as runPHP does, with the host paths under readOnly
// read-only. It returns the error of RunCLI.
func runSystemPHP(ctx context.Context, t *testing.T, code, readOnly string) (string, int, error) {
	t.Helper()
	var out bytes.Buffer
	exit, err := newTestEngine(t).RunCLI(ctx, gophper.Options{
		Args:   []string{"-r", code},
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig().WithDirMount("/", "/"),
		HostPath: func(path string) (string, bool, bool) {
			return path, readOnly == "" || !strings.HasPrefix(path, readOnly), true
		},
	})
	return out.String(), exit, err
}

// TestSystemPermissions changes owners and modes where PHP may not.
func TestSystemPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix permissions")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(ro, "kept")
	if err := os.WriteFile(kept, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "own")
	if err := os.WriteFile(own, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		code string
		want string
		root bool // whether the test makes sense as root
	}{
		{"chmod read-only", fmt.Sprintf(`var_export(@chmod(%q, 0777)); echo ' ', error_get_last()['message'];`, kept), "false chmod(): Read-only file system", true},
		{"chown read-only", fmt.Sprintf(`var_export(@chown(%q, posix_getuid())); echo ' ', error_get_last()['message'];`, kept), "false chown(): Read-only file system", true},
		{"chown to root", fmt.Sprintf(`var_export(@chown(%q, 0)); echo ' ', error_get_last()['message'];`, own), "false chown(): Operation not permitted", false},
		{"fileowner", fmt.Sprintf(`echo fileowner(%q) === posix_getuid() ? 'mine' : 'not mine';`, kept), "mine", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.root && os.Getuid() == 0 {
				t.Skip("root may give files away")
			}
			out, exit, err := runSystemPHP(context.Background(), t, tt.code, ro)
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || strings.TrimSpace(out) != tt.want {
				t.Errorf("exit %d, got %q, want %q", exit, out, tt.want)
			}
		})
	}
	if st, err := os.Stat(kept); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("the read-only file changed: %v %v", st.Mode(), err)
	}
}

// TestSystemLockInstances contends for one file from two instances.
func TestSystemLockInstances(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs flock")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	tried := filepath.Join(dir, "tried")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The holder keeps its lock until the other has tried, then exits
	// without unlocking: the end of the run releases it.
	holder := fmt.Sprintf(`
		$f = fopen(%q, 'r'); var_export(flock($f, LOCK_EX)); echo "\n";
		file_put_contents(%q, '');
		$end = microtime(true) + 10;
		while (!file_exists(%q) && microtime(true) < $end) { usleep(5000); clearstatcache(); }
		echo "leaving\n";`, file, filepath.Join(dir, "held"), tried)
	waiter := fmt.Sprintf(`
		$end = microtime(true) + 10;
		while (!file_exists(%q) && microtime(true) < $end) { usleep(5000); clearstatcache(); }
		$f = fopen(%q, 'r');
		var_export(flock($f, LOCK_EX | LOCK_NB, $would)); echo " $would\n";
		file_put_contents(%q, '');
		var_export(flock($f, LOCK_EX)); echo "\n";`, filepath.Join(dir, "held"), file, tried)
	type result struct {
		out  string
		exit int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		out, exit, err := runSystemPHP(context.Background(), t, holder, "")
		done <- result{out, exit, err}
	}()
	out, exit, err := runSystemPHP(context.Background(), t, waiter, "")
	if err != nil || exit != 0 || out != "false 1\ntrue\n" {
		t.Errorf("waiter: exit %d, %v\n%s", exit, err, out)
	}
	if r := <-done; r.err != nil || r.exit != 0 || r.out != "true\nleaving\n" {
		t.Errorf("holder: exit %d, %v\n%s", r.exit, r.err, r.out)
	}
}

// TestSystemLockCanceled ends a run while flock waits.
func TestSystemLockCanceled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs flock")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, ready, canceled := systemCancelWhenReady(t)
	out, _, err := runSystemPHP(ctx, t, fmt.Sprintf(`
		$a = fopen(%[1]q, 'r'); $b = fopen(%[1]q, 'r');
		flock($a, LOCK_EX); echo "locked\n"; touch(%[2]q);
		flock($b, LOCK_EX); echo "not canceled\n";`, file, ready), "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want the cancel", err)
	}
	if d := time.Since(canceled()); d > 2*time.Second {
		t.Errorf("the run took %v after the cancel", d)
	}
	if !strings.HasPrefix(out, "locked\n") {
		t.Errorf("got %q", out)
	}
}

// systemCancelWhenReady returns a context canceled 100ms after the script
// creates the file at ready, and when it was canceled. A deadline would
// count the time an instance takes to start, which a slow machine makes
// longer than the deadline.
func systemCancelWhenReady(t *testing.T) (ctx context.Context, ready string, canceled func() time.Time) {
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
