//go:build windows

//declscope:namespace system

package hostsys

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSystemLockFileWindows takes LockFileEx locks through two handles, as
// two PHP fds hold two host files. Unlocking with no lock is no error, as
// with flock(2).
func TestSystemLockFileWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() }) // read-only
		return f
	}
	a, b := open(), open()
	if err := systemLockFile(b, systemLockUnlock); err != nil {
		t.Errorf("unlock with no lock: %v", err)
	}
	if err := systemLockFile(a, systemLockExclusive); err != nil {
		t.Fatal(err)
	}
	if err := systemLockFile(b, systemLockShared); !errors.Is(err, errSystemLockBusy) {
		t.Errorf("shared over exclusive: %v", err)
	}
	// Converting a held lock unlocks first, as flock(2) does.
	if err := systemLockFile(a, systemLockShared); err != nil {
		t.Errorf("exclusive to shared: %v", err)
	}
	if err := systemLockFile(b, systemLockShared); err != nil {
		t.Errorf("shared beside shared: %v", err)
	}
}

// TestSystemLockFileClosedWindows fails on a closed file, whose handle is
// no longer valid, with the error of that and not as a busy lock.
func TestSystemLockFileClosedWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := systemLockFile(f, systemLockExclusive); err == nil || errors.Is(err, errSystemLockBusy) {
		t.Errorf("lock of a closed file: %v", err)
	}
}
