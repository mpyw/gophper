package gophper

import (
	"io"

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
}
