package server

import (
	"io"
	"time"
)

// PHPConfig is how requests run, for HTTPHandler and FastCGIServer.
type PHPConfig struct {
	// Mounts are the host directories PHP may access. Each appears inside
	// PHP at its own host path, so paths mean the same on both sides.
	Mounts []Mount
	// TempDir is mounted at /tmp inside PHP. Empty means os.TempDir().
	TempDir string
	// Concurrency limits PHP instances running at once (php-fpm's
	// pm.max_children). Zero means runtime.NumCPU().
	Concurrency int
	// MaxWaitTime is how long a request waits for a free instance before it
	// fails with 503. Zero means no limit.
	MaxWaitTime time.Duration
	// INI holds php.ini lines, such as "max_execution_time=30".
	INI []string
	// Env holds environment variables for PHP, as KEY=VALUE. The host
	// environment is not passed (php-fpm's clear_env).
	Env []string
	// ErrorLog receives PHP's stderr and gophper's own errors. Nil means os.Stderr.
	ErrorLog io.Writer
	// AccessLog receives one line per request. Nil means none.
	AccessLog io.Writer
}

// HTTPConfig configures an HTTPHandler.
type HTTPConfig struct {
	PHPConfig
	// Root is the document root: the host directory URLs map to. It must be
	// inside a mount. If there are no mounts, Root is mounted.
	Root string
	// Router is a script that runs for every request, as with php -S. It
	// runs from the directory the server started in, and $_SERVER describes
	// the script the request resolves to. Relative paths are inside Root.
	Router string
	// Index lists the files a directory runs. Default: index.php.
	Index []string
	// FrontController runs for paths that are not files. Default: /index.php.
	FrontController string
	// NoFrontController answers those paths with 404 instead.
	NoFrontController bool
	// SplitPath lists the suffixes that end a script's path. Default: .php.
	SplitPath []string
	// NoStatic answers paths to files other than scripts with 404.
	NoStatic bool
	// MaxBodySize limits request bodies. Default: 64 MiB. Negative means no limit.
	MaxBodySize int64
}

// FastCGIConfig configures a FastCGIServer.
type FastCGIConfig struct {
	PHPConfig
	// LimitExtensions lists the script extensions that may run (php-fpm's
	// security.limit_extensions). Default: .php and .phar.
	LimitExtensions []string
	// AllowedClients lists the addresses or prefixes that may connect over
	// TCP (php-fpm's listen.allowed_clients). Empty allows any.
	AllowedClients []string
	// PingPath answers "pong" (php-fpm's ping.path). Empty disables it.
	PingPath string
	// StatusPath answers counters as text (php-fpm's pm.status_path).
	// Empty disables it.
	StatusPath string
}
