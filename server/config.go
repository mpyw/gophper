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
	// environment is not passed (php-fpm's clear_env). A request's own CGI
	// variables, such as SERVER_NAME and QUERY_STRING, are dropped: each
	// request sets them.
	Env []string
	// ErrorLog receives PHP's stderr and gophper's own errors. Nil means os.Stderr.
	ErrorLog io.Writer
	// AccessLog receives one line per request. Nil means none.
	AccessLog io.Writer
	// OpcacheDir keeps opcache's compiled scripts in files. A worker also
	// keeps them in its own shared memory, which ends with the worker, and
	// a fresh instance starts with none. The files outlive both. Each build
	// of the PHP binaries gets a directory inside. Default: "gophper/opcache"
	// in the user cache directory. PHP runs whatever it finds there, so
	// anyone who can write to it can run code in every pool that uses it.
	OpcacheDir string
	// NoOpcache leaves opcache off.
	NoOpcache bool
	// NoWorkers starts a fresh PHP instance for each request. By default, a
	// worker serves many requests, one at a time, as a php-fpm child does,
	// and PHP resets its state between them.
	NoWorkers bool
	// MaxRequests is how many requests a worker serves before it is
	// replaced (php-fpm's pm.max_requests). Zero means 500.
	MaxRequests int
	// NoProcesses stops PHP from starting host programs (proc_open, exec
	// and the rest). Like php-fpm, PHP may start them by default. A child
	// runs outside the mounts, with the rights of the server.
	NoProcesses bool
	// NoNetwork stops PHP from using TCP, UDP and DNS: database servers,
	// HTTP clients, mail. Like php-fpm, PHP may use them by default. Unix
	// sockets inside the mounts still work.
	NoNetwork bool
	// MemoryLimit caps each PHP instance's linear memory, in bytes, which
	// memory_limit in php.ini cannot lift. A worker's opcache takes 64 MB
	// of it, so a worker needs about 96 MB. A cap PHP cannot start under is
	// refused at once. Zero means
	// none below WebAssembly's own 4 GiB.
	MemoryLimit int64
}

// HTTPConfig configures an HTTPHandler.
type HTTPConfig struct {
	PHPConfig
	// Root is the document root: the host directory URLs map to. It must be
	// inside a mount. If there are no mounts, Root is mounted.
	Root string
	// Router is a script that runs for every request, as with php -S. It
	// runs from the directory the server started in, and $_SERVER describes
	// the script the request resolves to. If it returns false, that script
	// runs, or the file is sent. Relative paths are inside Root.
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
