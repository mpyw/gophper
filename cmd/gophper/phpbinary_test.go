//declscope:namespace main

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestPHPBinaryScriptFallback writes the script in a private directory of
// the temporary directory, and refuses one that others can write to.
func TestPHPBinaryScriptFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no PHP_BINARY on Windows")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
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
	if runtime.GOOS == "windows" {
		t.Skip("no PHP_BINARY on Windows")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A cache directory that is a file.
	if _, err := phpBinaryScript(file, nil); err == nil {
		t.Error("a file as the cache directory was accepted")
	}
	// No temporary directory.
	t.Setenv("TMPDIR", filepath.Join(file, "tmp"))
	if _, err := phpBinaryScript("", nil); err == nil {
		t.Error("a missing temporary directory was accepted")
	}
}
