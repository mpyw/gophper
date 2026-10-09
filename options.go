package gophper

import (
	"io"
	"os"

	"github.com/tetratelabs/wazero"
)

// Options configures a single PHP run, for the CLI or the CGI SAPI.
type Options struct {
	// Args are passed to PHP, excluding argv[0].
	Args []string
	// Env holds environment variables in "KEY=VALUE" form.
	Env []string
	// Dir is the working directory PHP starts in. It must be inside FS.
	// Empty means "/". WASI has no inherited working directory.
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// FS is exposed to PHP. Nil means no file system access.
	FS wazero.FSConfig
	// HostPath maps a path PHP sees to the host file behind it. WASI lacks
	// permissions, owners and locks, so the host does those on that file.
	// Child processes get files and their working directory through it too.
	// It reports whether PHP may change the file, and ok is false for a path
	// with no host file behind it. Nil means no path has one.
	HostPath func(path string) (host string, writable, ok bool)
	// Processes lets PHP start host programs: proc_open, exec and the rest.
	// A child runs outside FS, with the rights of the Go process.
	Processes bool
	// Functions are PHP functions written in Go, by name. Each becomes an
	// internal function, which a script calls as any other.
	Functions map[string]Function
	// Signals delivers signals to PHP, as from signal.Notify. A signal the
	// script handles, with pcntl_signal(), runs the handler. One it ignores
	// is dropped. Any other ends the run, with exit code 128 plus the
	// signal's Linux number, as its default action would.
	Signals <-chan os.Signal
}
