//declscope:namespace main

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// phpBinaryScript writes a script that runs "gophper php" with the same
// global options, and returns its path for EngineConfig.PHPBinary. PHP_BINARY
// then names it, so that Composer, Laravel and others that start PHP again
// start gophper. It returns "" where there is no /bin/sh.
//
// A script, not a symlink to gophper: PHP resolves PHP_BINARY with
// realpath, which would drop the name "php" that selects the subcommand.
func phpBinaryScript(cacheDir string, args []string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	quoted := []string{phpBinaryQuote(exe)}
	for _, a := range args {
		quoted = append(quoted, phpBinaryQuote(a))
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s php \"$@\"\n", strings.Join(quoted, " "))

	base := cacheDir
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("gophper-%d", os.Getuid()))
		// Anyone can create this name first, and then replace the script
		// between our write and PHP's exec. So only a private directory of
		// our own will do.
		if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		fi, err := os.Lstat(base)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || !phpBinaryOwned(fi) {
			return "", fmt.Errorf("%s: not a private directory of this user", base)
		}
	}
	sum := sha256.Sum256([]byte(script))
	dir := filepath.Join(base, "bin", hex.EncodeToString(sum[:8]))
	path := filepath.Join(dir, "php")
	if b, err := os.ReadFile(path); err == nil && string(b) == script {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Written to a temporary name first, so that a concurrent run never
	// executes half a script.
	tmp, err := os.CreateTemp(dir, ".php-*")
	if err != nil {
		return "", err
	}
	// After the rename the name is gone, and a failed removal leaves only
	// a stray temporary file.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(script); err != nil {
		return "", errors.Join(err, tmp.Close())
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// phpBinaryQuote quotes s for /bin/sh.
func phpBinaryQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
