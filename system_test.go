package gophper_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
		{"hostname", `echo gethostname() === php_uname('n') && gethostname() !== '' ? 'same' : 'differ';`, "same"},
		{"fileperms", fmt.Sprintf(`printf('%%o', fileperms(%q) & 0777);`, file), "750"},
		{"fileowner", fmt.Sprintf(`echo fileowner(%q);`, file), fmt.Sprint(os.Getuid())},
		{"chmod", fmt.Sprintf(`chmod(%[1]q, 0640); clearstatcache(); printf('%%o', fileperms(%[1]q) & 0777);`, file), "640"},
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
