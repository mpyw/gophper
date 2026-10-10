//go:build unix

//declscope:namespace main

package main

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestPHPBinaryTempBaseGone: the directory is gone by the time it is
// checked, as when another process removes it.
func TestPHPBinaryTempBaseGone(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	lstat := phpBinaryLstat
	t.Cleanup(func() { phpBinaryLstat = lstat })
	phpBinaryLstat = func(name string) (fs.FileInfo, error) {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		return lstat(name)
	}
	if base, err := phpBinaryTempBase(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s, %v", base, err)
	}
}
