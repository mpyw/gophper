// Package gophpercaddy serves PHP from Caddy, with no cgo.
//
// It registers the http.handlers.gophper module, the gophper Caddyfile
// directive and the php-server command:
//
//	example.com {
//		gophper ./public
//	}
//
// The handler routes like Caddy's php_server: .php files run, other files
// are sent as they are, and every other path runs the front controller.
package gophpercaddy

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is the http.handlers.gophper module. It answers every request
// itself, so it ends the route.
type Handler struct {
	// Root is the document root. Default: the current directory.
	Root string `json:"root,omitempty"`
	// Mounts are the directories PHP may access, at the same paths.
	// Default: Root.
	Mounts []HandlerMount `json:"mounts,omitempty"`
	// Router runs for every request, as with php -S.
	Router string `json:"router,omitempty"`
	// Index lists the files a directory runs. Default: index.php.
	Index []string `json:"index,omitempty"`
	// FrontController runs for paths that are not files. Default: index.php.
	FrontController string `json:"front_controller,omitempty"`
	// NoFrontController answers those paths with 404.
	NoFrontController bool `json:"no_front_controller,omitempty"`
	// SplitPath lists the suffixes that end a script's path. Default: .php.
	SplitPath []string `json:"split_path,omitempty"`
	// NoStatic answers files other than scripts with 404.
	NoStatic bool `json:"no_static,omitempty"`
	// MaxBodySize limits request bodies, in bytes. Default: 64 MiB. Negative: no limit.
	MaxBodySize int64 `json:"max_body_size,omitempty"`
	// TempDir is mounted at /tmp inside PHP. Default: the system's.
	TempDir string `json:"temp_dir,omitempty"`
	// Concurrency limits PHP instances at once. Default: the number of CPUs.
	Concurrency int `json:"concurrency,omitempty"`
	// MaxWaitTime is how long a request waits for a free instance before 503.
	MaxWaitTime caddy.Duration `json:"max_wait_time,omitempty"`
	// INI holds php.ini lines such as "max_execution_time=30".
	INI []string `json:"ini,omitempty"`
	// Env holds environment variables for PHP, as KEY=VALUE.
	Env []string `json:"env,omitempty"`

	handler *server.HTTPHandler
}

// HandlerMount is a directory PHP may access.
type HandlerMount struct {
	Dir      string `json:"dir"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.gophper",
		New: func() caddy.Module { return new(Handler) },
	}
}

// handlerEngine is shared by every handler and every config reload.
// Compiling the PHP binaries takes seconds, so it happens once per process.
var handlerEngine = sync.OnceValues(func() (*gophper.Engine, error) {
	return gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
})

// Provision prepares the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	engine, err := handlerEngine()
	if err != nil {
		return err
	}
	root := h.Root
	if root == "" {
		root = "."
	}
	cfg := server.HTTPConfig{
		PHPConfig: server.PHPConfig{
			TempDir:     h.TempDir,
			Concurrency: h.Concurrency,
			MaxWaitTime: time.Duration(h.MaxWaitTime),
			INI:         h.INI,
			Env:         h.Env,
			ErrorLog:    zap.NewStdLog(ctx.Logger()).Writer(),
		},
		Root:              root,
		Router:            h.Router,
		Index:             h.Index,
		FrontController:   h.FrontController,
		NoFrontController: h.NoFrontController,
		SplitPath:         h.SplitPath,
		NoStatic:          h.NoStatic,
		MaxBodySize:       h.MaxBodySize,
	}
	for _, m := range h.Mounts {
		dir, err := filepath.Abs(m.Dir)
		if err != nil {
			return err
		}
		cfg.Mounts = append(cfg.Mounts, server.Mount{Dir: dir, ReadOnly: m.ReadOnly})
	}
	h.handler, err = server.NewHTTPHandler(engine, cfg)
	return err
}

// Cleanup releases the handler when its config is unloaded.
func (h *Handler) Cleanup() error {
	if h.handler == nil {
		return nil
	}
	return h.handler.Close()
}

// ServeHTTP serves the request. It never calls next.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	h.handler.ServeHTTP(w, r)
	return nil
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
