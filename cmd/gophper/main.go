// Command gophper runs PHP on wazero.
//
//	gophper php [php options] [file] [args...]   the php CLI
//	gophper serve [options]                      an HTTP(S) server for a PHP app
//	gophper fcgi [options]                       a FastCGI server, like php-fpm
//
// Invoked under the name "php" (through a symlink, for example), it behaves
// like "gophper php".
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/urfave/cli/v3"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/server"
)

func main() {
	args := os.Args
	if strings.TrimSuffix(filepath.Base(args[0]), ".exe") == "php" {
		args = append([]string{args[0], "php"}, args[1:]...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCommand().Run(ctx, args)
	stop()

	var exit cli.ExitCoder
	switch {
	case errors.As(err, &exit):
		if msg := exit.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, "gophper:", msg)
		}
		os.Exit(exit.ExitCode())
	case err != nil:
		fmt.Fprintln(os.Stderr, "gophper:", err)
		os.Exit(1)
	}
}

func newRootCommand() *cli.Command {
	return &cli.Command{
		Name:  "gophper",
		Usage: "run PHP on WebAssembly, with no cgo",
		// main prints errors and exits; urfave/cli must not do it as well.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands: []*cli.Command{
			{
				Name:      "php",
				Aliases:   []string{"cli"},
				Usage:     "run the php CLI; every argument goes to PHP",
				ArgsUsage: "[php options] [file] [args...]",
				// "-r", "-d" and the rest belong to PHP, not to gophper.
				SkipFlagParsing: true,
				Action:          phpAction,
			},
			{
				Name:  "serve",
				Usage: "serve a PHP app over HTTP or HTTPS, with no web server in front",
				Description: "Existing .php files run, other files are sent as they are,\n" +
					"and every other path runs the root's index.php.",
				Flags: append([]cli.Flag{
					&cli.StringFlag{
						Name:  "listen",
						Value: "127.0.0.1:8080",
						Usage: "TCP address to listen on",
					},
					&cli.StringFlag{
						Name:  "tls-cert",
						Usage: "certificate file; serves HTTPS with --tls-key",
					},
					&cli.StringFlag{
						Name:  "tls-key",
						Usage: "private key file for --tls-cert",
					},
				}, cgiFlags()...),
				Action: serveAction,
			},
			{
				Name:  "fcgi",
				Usage: "serve PHP over FastCGI, like php-fpm",
				Flags: append([]cli.Flag{
					&cli.StringFlag{
						Name:  "listen",
						Value: "127.0.0.1:9000",
						Usage: `TCP address, or "unix:/path/to.sock"`,
					},
				}, cgiFlags()...),
				Action: fcgiAction,
			},
		},
	}
}

// cgiFlags are the options serve and fcgi share.
func cgiFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:  "root",
			Value: ".",
			Usage: "directory PHP may access, mounted at the same path",
		},
		&cli.IntFlag{
			Name:  "workers",
			Usage: "max concurrent PHP instances (0 = number of CPUs)",
		},
		&cli.StringSliceFlag{
			Name:    "define",
			Aliases: []string{"d"},
			Usage:   "php.ini entry as key=value, such as max_execution_time=30 (repeatable)",
			Validator: func(entries []string) error {
				for _, e := range entries {
					if !strings.Contains(e, "=") {
						return fmt.Errorf("want key=value, got %q", e)
					}
				}
				return nil
			},
		},
	}
}

// phpAction mounts the host file system at "/" and starts in the current directory.
func phpAction(ctx context.Context, cmd *cli.Command) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())
	code, err := engine.RunCLI(ctx, gophper.Options{
		Args:   cmd.Args().Slice(),
		Env:    os.Environ(),
		Dir:    wd,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		FS:     wazero.NewFSConfig().WithDirMount("/", "/"),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return cli.Exit("", code)
	}
	return nil
}

func serveAction(ctx context.Context, cmd *cli.Command) error {
	cert, key := cmd.String("tls-cert"), cmd.String("tls-key")
	if (cert == "") != (key == "") {
		return errors.New("--tls-cert and --tls-key go together")
	}

	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())

	h, err := server.NewHTTPHandler(engine, server.HTTPConfig{
		Root:    cmd.String("root"),
		Workers: cmd.Int("workers"),
		INI:     cmd.StringSlice("define"),
	})
	if err != nil {
		return err
	}
	defer h.Close()

	l, err := net.Listen("tcp", cmd.String("listen"))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	scheme := "http"
	if cert != "" {
		scheme = "https"
	}
	fmt.Fprintf(os.Stderr, "gophper: serving %s on %s://%s\n", cmd.String("root"), scheme, l.Addr())
	if cert != "" {
		err = srv.ServeTLS(l, cert, key)
	} else {
		err = srv.Serve(l)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func fcgiAction(ctx context.Context, cmd *cli.Command) error {
	root, err := filepath.Abs(cmd.String("root"))
	if err != nil {
		return err
	}

	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())

	listen := cmd.String("listen")
	network, address := "tcp", listen
	if path, ok := strings.CutPrefix(listen, "unix:"); ok {
		network, address = "unix", path
		os.Remove(path)
	}
	l, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "gophper: FastCGI on %s, root %s\n", listen, root)

	srv := &server.FastCGIServer{
		Engine:  engine,
		Root:    root,
		Workers: cmd.Int("workers"),
		INI:     cmd.StringSlice("define"),
	}
	return srv.Serve(ctx, l)
}
