//declscope:namespace main

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// phpBinaryArgsFile holds the global options of a php.exe that
// phpBinaryScript made, one per line, next to it.
const phpBinaryArgsFile = "gophper.args"

// phpBinaryScript makes a program that runs "gophper php" with the same
// global options, and returns its path for EngineConfig.PHPBinary. PHP_BINARY
// then names it, so that Composer, Laravel and others that start PHP again
// start gophper.
//
// On Unix it is a shell script. A symlink to gophper would not do: PHP
// resolves PHP_BINARY with realpath, which would drop the name "php" that
// selects the subcommand. On Windows it is gophper itself, as php.exe, with
// the options in phpBinaryArgsFile. A .cmd script would do on its own, but
// Windows passes a batch file's arguments through cmd.exe, which reads &
// and | in them.
func phpBinaryScript(cacheDir string, args []string) (string, error) {
	exe, err := phpBinaryExecutable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	base, err := phpBinaryBase(cacheDir)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return phpBinaryExe(base, exe, args)
	}
	quoted := []string{phpBinaryQuote(exe)}
	for _, a := range args {
		quoted = append(quoted, phpBinaryQuote(a))
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s php \"$@\"\n", strings.Join(quoted, " "))
	sum := sha256.Sum256([]byte(script))
	path := filepath.Join(base, "bin", hex.EncodeToString(sum[:8]), "php")
	if b, err := os.ReadFile(path); err == nil && string(b) == script {
		return path, nil
	}
	return path, phpBinaryWrite(path, 0o755, func(w io.Writer) error {
		_, err := io.WriteString(w, script)
		return err
	})
}

// phpBinaryExecutable is a var so that a test can lose gophper's path, as
// an upgrade that removes the running binary does.
var phpBinaryExecutable = os.Executable

// phpBinaryBase is the cache directory, or phpBinaryTempBase without one.
func phpBinaryBase(cacheDir string) (string, error) {
	if cacheDir != "" {
		return cacheDir, nil
	}
	return phpBinaryTempBase()
}

// phpBinaryRename is a var so that a test can lose the race with another
// run, which makes php.exe between our check and our rename.
var phpBinaryRename = os.Rename

// phpBinaryExe links or copies exe to php.exe, in a directory of its own
// for each build of gophper and each set of options.
func phpBinaryExe(base, exe string, args []string) (string, error) {
	content := phpBinaryArgsText(args)
	// Already one: a php.exe that starts PHP again is the same program.
	if filepath.Base(exe) == "php.exe" {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), phpBinaryArgsFile)); err == nil && string(b) == content {
			return exe, nil
		}
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	// A rebuilt gophper at the same path gets a new copy.
	key := strings.Join(append([]string{exe, strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10)}, args...), "\x00")
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(base, "bin", hex.EncodeToString(sum[:8]))
	path := filepath.Join(dir, "php.exe")
	argsPath := filepath.Join(dir, phpBinaryArgsFile)
	// What is there is used only if it is this gophper, with these options.
	if phpBinarySame(exe, path) && phpBinaryHas(argsPath, content) {
		return path, nil
	}
	// The options first: a php.exe that exists has them. A php.exe that
	// runs may be reading them, so equal ones are left as they are.
	if !phpBinaryHas(argsPath, content) {
		if err := phpBinaryWrite(argsPath, 0o644, func(w io.Writer) error {
			_, err := io.WriteString(w, content)
			return err
		}); err != nil && !phpBinaryHas(argsPath, content) {
			return "", err
		}
	}
	tmp := filepath.Join(dir, ".php-"+strconv.Itoa(os.Getpid())+".exe")
	if err := os.Link(exe, tmp); err != nil {
		// Another volume: a copy.
		if err := phpBinaryCopy(exe, tmp); err != nil {
			return "", err
		}
	}
	if err := phpBinaryRename(tmp, path); err != nil {
		// Gone after a successful rename; otherwise only a stray file.
		_ = os.Remove(tmp)
		// Another run made it first, and it may be running already.
		if phpBinarySame(exe, path) {
			return path, nil
		}
		return "", err
	}
	return path, nil
}

// phpBinaryArgsText is the content of phpBinaryArgsFile for args.
func phpBinaryArgsText(args []string) string {
	var b strings.Builder
	for _, a := range args {
		_, _ = b.WriteString(a + "\n") // a strings.Builder never fails
	}
	return b.String()
}

// phpBinaryHas reports whether the file at path holds content.
func phpBinaryHas(path, content string) bool {
	b, err := os.ReadFile(path)
	return err == nil && string(b) == content
}

// phpBinarySame reports whether path is exe: its link, or a copy of the
// same size. Reading 40 MB to compare would cost every run, and the
// directory is the user's own, so a copy is taken on its size.
func phpBinarySame(exe, path string) bool {
	a, err := os.Stat(exe)
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && b.Mode().IsRegular() && (os.SameFile(a, b) || a.Size() == b.Size())
}

func phpBinaryCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	// Opened read-only, so closing it loses nothing.
	defer func() { _ = in.Close() }()
	return phpBinaryWrite(dst, 0o755, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// phpBinaryClose is a var so that a test can fail it, as NFS may with a
// write it deferred to the close.
var phpBinaryClose = (*os.File).Close

// phpBinaryWrite writes a file under a temporary name, then renames it, so
// that a concurrent run never sees half of it.
func phpBinaryWrite(path string, mode fs.FileMode, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".php-*")
	if err != nil {
		return err
	}
	// After the rename the name is gone, and a failed removal leaves only
	// a stray temporary file.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := write(tmp); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Chmod(mode); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := phpBinaryClose(tmp); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// phpBinaryArgs returns the global options of the php.exe that is running,
// from phpBinaryArgsFile next to it, or none. Only Windows writes one, so
// a php symlink on Unix reads no stray file beside it.
func phpBinaryArgs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	exe, err := phpBinaryExecutable()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), phpBinaryArgsFile))
	if err != nil {
		return nil
	}
	return phpBinaryParseArgs(string(b))
}

// phpBinaryParseArgs reads one option per line. An editor may have left
// CRLF, or blank lines.
func phpBinaryParseArgs(text string) []string {
	var args []string
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSuffix(line, "\r"); line != "" {
			args = append(args, line)
		}
	}
	return args
}

// phpBinaryQuote quotes s for /bin/sh.
func phpBinaryQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
