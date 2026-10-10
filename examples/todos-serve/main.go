// Command todos-serve serves the Laravel app in ../todos over HTTP with
// gophper's server package, as gophper serve does.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	app := flag.String("app", "todos", "the Laravel app directory")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *listen, *app); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, listen, app string) (err error) {
	// A mount is an absolute path, and PHP sees it at the same path.
	app, err = filepath.Abs(app)
	if err != nil {
		return err
	}

	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close(context.Background())) }()

	h, err := server.NewHTTPHandler(engine, server.HTTPConfig{
		PHPConfig: server.PHPConfig{
			// PHP reaches vendor/, storage/ and .env outside public/.
			Mounts:    []server.Mount{{Dir: app}},
			AccessLog: os.Stdout,
		},
		Root: filepath.Join(app, "public"),
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, h.Close()) }()

	return serve(ctx, listen, h)
}

// serve runs an HTTP server until ctx is done, then shuts it down.
func serve(ctx context.Context, listen string, h http.Handler) error {
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("listening on http://%s/", listen)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
