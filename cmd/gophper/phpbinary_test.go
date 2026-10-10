//declscope:namespace main

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestPHPBinaryScriptFallback writes the script in a private directory of
// the temporary directory, and refuses one that others can write to.
func TestPHPBinaryScriptFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if runtime.GOOS == "windows" {
		// The user's local application data, with php.exe and its options.
		t.Setenv("LOCALAPPDATA", tmp)
		path, err := phpBinaryScript("", []string{"--no-cache"})
		if err != nil {
			t.Fatal(err)
		}
		if rel, err := filepath.Rel(filepath.Join(tmp, "gophper"), path); err != nil || strings.HasPrefix(rel, "..") || filepath.Base(path) != "php.exe" {
			t.Errorf("%q is not php.exe under %q", path, tmp)
		}
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(path), phpBinaryArgsFile)); err != nil || string(b) != "--no-cache\n" {
			t.Errorf("options %q, %v", b, err)
		}
		return
	}
	path, err := phpBinaryScript("", nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(tmp, fmt.Sprintf("gophper-%d", os.Getuid()))
	if rel, err := filepath.Rel(base, path); err != nil || rel == ".." || filepath.IsAbs(rel) {
		t.Errorf("script %q is outside %q", path, base)
	}
	fi, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("%s: mode %o, want 700", base, perm)
	}
	// Others may write to it: someone else could have made it.
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := phpBinaryScript("", nil); err == nil {
		t.Error("a directory others can write to was accepted")
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink to a private directory is no private directory itself.
	if err := os.Rename(base, base+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base+".real", base); err != nil {
		t.Fatal(err)
	}
	if _, err := phpBinaryScript("", nil); err == nil {
		t.Error("a symlink was accepted")
	}
}

// TestPHPBinaryScriptErrors fails where the directory cannot be made.
func TestPHPBinaryScriptErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A cache directory that is a file.
	if _, err := phpBinaryScript(file, nil); err == nil {
		t.Error("a file as the cache directory was accepted")
	}
	if runtime.GOOS == "windows" {
		// No local application data to put it in.
		t.Setenv("LOCALAPPDATA", "")
		if _, err := phpBinaryScript("", nil); err == nil {
			t.Error("no local application data was accepted")
		}
		return
	}
	// No temporary directory.
	t.Setenv("TMPDIR", filepath.Join(file, "tmp"))
	if _, err := phpBinaryScript("", nil); err == nil {
		t.Error("a missing temporary directory was accepted")
	}
}

// TestPHPBinaryExe makes gophper's php.exe with its options, once for each
// build and set of options. It runs on every OS, though only Windows uses it.
func TestPHPBinaryExe(t *testing.T) {
	base := t.TempDir()
	exe := filepath.Join(t.TempDir(), "gophper.exe")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"--cache-dir", `C:\cache`}
	path, err := phpBinaryExe(base, exe, args)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "binary" || filepath.Base(path) != "php.exe" {
		t.Errorf("%s: %q, %v", path, b, err)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(path), phpBinaryArgsFile)); err != nil || string(b) != "--cache-dir\nC:\\cache\n" {
		t.Errorf("options %q, %v", b, err)
	}
	if again, err := phpBinaryExe(base, exe, args); err != nil || again != path {
		t.Errorf("again: %s, %v", again, err)
	}
	if other, err := phpBinaryExe(base, exe, nil); err != nil || other == path {
		t.Errorf("other options: %s, %v", other, err)
	}
	// Another volume: copied, not linked.
	copied := filepath.Join(t.TempDir(), "copy")
	if err := phpBinaryCopy(exe, copied); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(copied); err != nil || string(b) != "binary" {
		t.Errorf("copy %q, %v", b, err)
	}
	// A link that cannot be made, as across volumes: a copy instead. The
	// temporary name taken already stands in for the failure.
	other := []string{"--other"}
	blockDir := t.TempDir()
	if _, err := phpBinaryExe(blockDir, exe, other); err != nil {
		t.Fatal(err)
	}
	blocked, err := os.ReadDir(filepath.Join(blockDir, "bin"))
	if err != nil || len(blocked) != 1 {
		t.Fatalf("%v, %v", blocked, err)
	}
	made := filepath.Join(blockDir, "bin", blocked[0].Name())
	if err := os.Remove(filepath.Join(made, "php.exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(made, ".php-"+strconv.Itoa(os.Getpid())+".exe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := phpBinaryExe(blockDir, exe, other); err != nil {
		t.Errorf("copy: %v", err)
	} else if b, err := os.ReadFile(path); err != nil || string(b) != "binary" {
		t.Errorf("copied %q, %v", b, err)
	}
	// A file where the directory would go.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := phpBinaryExe(file, exe, nil); err == nil {
		t.Error("a file as the directory was accepted")
	}
	if _, err := phpBinaryExe(base, filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("a missing gophper was accepted")
	}
	if err := phpBinaryCopy(filepath.Join(t.TempDir(), "missing"), copied); err == nil {
		t.Error("copied a missing file")
	}
}

// TestPHPBinaryExeReuse uses a php.exe that runs with these options as it
// is, and keeps an equal gophper.args, which a running php.exe may read.
func TestPHPBinaryExeReuse(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "php.exe")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"--no-cache"}
	if err := os.WriteFile(filepath.Join(dir, phpBinaryArgsFile), []byte(phpBinaryArgsText(args)), 0o644); err != nil {
		t.Fatal(err)
	}
	if path, err := phpBinaryExe(t.TempDir(), exe, args); err != nil || path != exe {
		t.Errorf("a php.exe with these options: %s, %v", path, err)
	}
	base := t.TempDir()
	path, err := phpBinaryExe(base, exe, nil)
	if err != nil || path == exe {
		t.Fatalf("other options: %s, %v", path, err)
	}
	argsFile := filepath.Join(filepath.Dir(path), phpBinaryArgsFile)
	before, err := os.Stat(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	// Made again, as after the copy was removed: gophper.args stays.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := phpBinaryExe(base, exe, nil); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(argsFile); err != nil || !os.SameFile(before, after) {
		t.Errorf("gophper.args was written again: %v", err)
	}
}

func TestPHPBinaryParseArgs(t *testing.T) {
	got := phpBinaryParseArgs("--cache-dir\r\nC:\\cache\r\n\r\n--no-cache\n")
	if want := []string{"--cache-dir", `C:\cache`, "--no-cache"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := phpBinaryParseArgs(""); got != nil {
		t.Errorf("empty: %q", got)
	}
}

// phpBinaryMade makes php.exe for exe under base, and returns the
// directory it went in.
func phpBinaryMade(t *testing.T, base, exe string) string {
	t.Helper()
	path, err := phpBinaryExe(base, exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(path)
}

// TestPHPBinaryExeBlocked fails where neither the link nor the copy can
// take php.exe's place: a directory is in the way of each.
func TestPHPBinaryExeBlocked(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "gophper.exe")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	block := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(path, "in-the-way"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// The temporary name: the link fails, and so does the copy's rename.
	base := t.TempDir()
	dir := phpBinaryMade(t, base, exe)
	if err := os.Remove(filepath.Join(dir, "php.exe")); err != nil {
		t.Fatal(err)
	}
	block(filepath.Join(dir, ".php-"+strconv.Itoa(os.Getpid())+".exe"))
	if _, err := phpBinaryExe(base, exe, nil); err == nil {
		t.Error("a directory at the temporary name was accepted")
	}

	// php.exe itself: the rename fails, and a directory is no php.exe.
	base = t.TempDir()
	dir = phpBinaryMade(t, base, exe)
	if err := os.Remove(filepath.Join(dir, "php.exe")); err != nil {
		t.Fatal(err)
	}
	block(filepath.Join(dir, "php.exe"))
	if _, err := phpBinaryExe(base, exe, nil); err == nil {
		t.Error("a directory at php.exe was accepted")
	}
	if left, err := filepath.Glob(filepath.Join(dir, ".php-*")); err != nil || len(left) != 0 {
		t.Errorf("left behind: %v, %v", left, err)
	}
}

// TestPHPBinaryExeRunning keeps a php.exe that runs, which Windows does not
// let a rename replace. Another run made it, from the same gophper.
func TestPHPBinaryExeRunning(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a rename replaces an open file on Unix")
	}
	exe := filepath.Join(t.TempDir(), "gophper.exe")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	dir := phpBinaryMade(t, base, exe)
	// Without its options, it is made again, and the rename finds it open.
	if err := os.Remove(filepath.Join(dir, phpBinaryArgsFile)); err != nil {
		t.Fatal(err)
	}
	running, err := os.Open(filepath.Join(dir, "php.exe"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Close() }() // read-only
	if path, err := phpBinaryExe(base, exe, nil); err != nil || path != filepath.Join(dir, "php.exe") {
		t.Errorf("%s, %v", path, err)
	}
}

// TestPHPBinarySameMissing is false for a gophper that is gone, as when it
// was replaced while running.
func TestPHPBinarySameMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "php.exe")
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if phpBinarySame(filepath.Join(t.TempDir(), "missing"), path) {
		t.Error("a missing gophper is the same as php.exe")
	}
}

// TestPHPBinaryWriteErrors leaves nothing at path when the temporary file
// cannot be made, written or given its mode.
func TestPHPBinaryWriteErrors(t *testing.T) {
	broken := errors.New("broken")
	path := filepath.Join(t.TempDir(), "file")
	if err := phpBinaryWrite(path, 0o644, func(io.Writer) error { return broken }); !errors.Is(err, broken) {
		t.Errorf("failed write: %v", err)
	}
	// The temporary file closed under it: the mode cannot be set.
	if err := phpBinaryWrite(path, 0o644, func(w io.Writer) error {
		f, ok := w.(*os.File)
		if !ok {
			return fmt.Errorf("%T is no file", w)
		}
		return f.Close()
	}); !errors.Is(err, os.ErrClosed) {
		t.Errorf("closed file: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: %v", path, err)
	}
	if left, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".php-*")); err != nil || len(left) != 0 {
		t.Errorf("left behind: %v, %v", left, err)
	}

	// A directory only others may write to. Windows ignores a read-only
	// directory, and root writes anywhere.
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		return
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so that TempDir can remove it
	if err := phpBinaryWrite(filepath.Join(dir, "file"), 0o644, func(io.Writer) error { return nil }); !errors.Is(err, os.ErrPermission) {
		t.Errorf("read-only directory: %v", err)
	}
}
