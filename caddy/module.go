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
// are sent as they are, and every other path runs the root's index.php.
package gophpercaddy

import (
	"context"
	"net/http"
	"sync"

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
	// Root is the document root, and the only directory PHP may access.
	// Default: the current directory.
	Root string `json:"root,omitempty"`
	// TempDir is mounted at /tmp inside PHP. Default: the system's.
	TempDir string `json:"temp_dir,omitempty"`
	// Workers limits concurrent PHP instances. Default: the number of CPUs.
	Workers int `json:"workers,omitempty"`
	// INI holds php.ini lines such as "max_execution_time=30".
	INI []string `json:"ini,omitempty"`

	handler *server.HTTPHandler
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.gophper",
		New: func() caddy.Module { return new(Handler) },
	}
}

// moduleEngine is shared by every handler and every config reload.
// Compiling the PHP binaries takes seconds, so it happens once per process.
var moduleEngine = sync.OnceValues(func() (*gophper.Engine, error) {
	return gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
})

// Provision prepares the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	engine, err := moduleEngine()
	if err != nil {
		return err
	}
	root := h.Root
	if root == "" {
		root = "."
	}
	h.handler, err = server.NewHTTPHandler(engine, server.HTTPConfig{
		Root:     root,
		TempDir:  h.TempDir,
		Workers:  h.Workers,
		INI:      h.INI,
		ErrorLog: zap.NewStdLog(ctx.Logger()).Writer(),
	})
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
