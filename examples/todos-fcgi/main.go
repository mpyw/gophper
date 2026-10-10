// Command todos-fcgi runs the Laravel app in ../todos as a FastCGI server,
// as gophper fcgi and php-fpm do. FrankenPHP serves HTTP in front of it,
// with the Caddyfile beside this file.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "address to listen on")
	app := flag.String("app", "todos", "the Laravel app directory")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *listen, *app); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, listen, app string) (err error) {
	// The web server sends SCRIPT_FILENAME as a host path. PHP sees the
	// mount at that same path.
	app, err = filepath.Abs(app)
	if err != nil {
		return err
	}

	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close(context.Background())) }()

	s, err := server.NewFastCGIServer(engine, server.FastCGIConfig{
		PHPConfig: server.PHPConfig{
			Mounts:    []server.Mount{{Dir: app}},
			AccessLog: os.Stdout,
		},
		PingPath: "/ping",
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()

	l, err := new(net.ListenConfig).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	log.Printf("FastCGI on %s, for %s", listen, app)
	return s.Serve(ctx, l)
}
