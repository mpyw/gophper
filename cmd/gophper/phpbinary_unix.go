//go:build unix

//declscope:namespace main

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// phpBinaryLstat is a var so that a test can remove the directory between
// the Mkdir and the Lstat, as another process may.
var phpBinaryLstat = os.Lstat

// phpBinaryTempBase is a directory of this user's own in the temporary
// directory. Anyone can create its name first, and then replace the script
// between our write and PHP's exec. So only a private directory of ours
// will do.
func phpBinaryTempBase() (string, error) {
	base := filepath.Join(os.TempDir(), fmt.Sprintf("gophper-%d", os.Getuid()))
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	fi, err := phpBinaryLstat(base)
	if err != nil {
		return "", err
	}
	st, owned := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || !owned || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s: not a private directory of this user", base)
	}
	return base, nil
}
