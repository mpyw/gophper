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
	// A child runs outside FS, with the rights of the Go process. It shares
	// a Stdin that is an *os.File, and reads the null device for any other.
	Processes bool
	// Network lets PHP use TCP, UDP and DNS, with the network of the Go
	// process. Without it, sockets fail with EACCES, and lookups fail as
	// a resolver that cannot be reached would. Unix sockets go through
	// HostPath instead, as files do.
	Network bool
	// MemoryLimit caps PHP's linear memory, in bytes: its code and data as
	// well as what the script allocates. A script that needs more ends with
	// "Out of memory", which memory_limit cannot lift. Zero means none
	// below WebAssembly's own 4 GiB.
	MemoryLimit int64
	// Functions are PHP functions written in Go, by name. Each becomes an
	// internal function, which a script calls as any other.
	Functions map[string]Function
	// Signals delivers signals to PHP, as from signal.Notify. A signal the
	// script handles, with pcntl_signal(), runs the handler. One it ignores
	// is dropped. Any other ends the run, with exit code 128 plus the
	// signal's Linux number, as its default action would.
	Signals <-chan os.Signal
}
