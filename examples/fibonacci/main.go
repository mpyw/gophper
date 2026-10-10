// Command fibonacci serves a PHP page that computes Fibonacci numbers by
// naive recursion twice: in PHP, and in Go through a Go function. It shows
// both results and timings with var_dump, as text/plain.
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/mpyw/gophper"
	"github.com/tetratelabs/wazero"
)

//go:embed index.php
var scripts embed.FS

func fib(n int64) int64 {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

var functions = map[string]gophper.Function{
	"go_fib": func(ctx context.Context, args []any) (any, error) {
		if len(args) != 1 {
			return nil, errors.New("go_fib() expects exactly 1 argument")
		}
		n, ok := args[0].(int64)
		if !ok {
			return nil, fmt.Errorf("go_fib(): argument #1 must be of type int, %T given", args[0])
		}
		return fib(n), nil
	},
}

// handler runs index.php with php-cgi for every request.
type handler struct {
	engine *gophper.Engine
}

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var stdout bytes.Buffer
	code, err := h.engine.RunCGI(r.Context(), gophper.Options{
		// php-cgi turns opcache on, which needs a writable /tmp. A fresh
		// instance per request would gain nothing from it.
		Args: []string{"-d", "opcache.enable=0"},
		Env: []string{
			"GATEWAY_INTERFACE=CGI/1.1",
			"SERVER_PROTOCOL=" + r.Proto,
			"REQUEST_METHOD=" + r.Method,
			"REQUEST_URI=" + r.URL.RequestURI(),
			"QUERY_STRING=" + r.URL.RawQuery,
			"SCRIPT_NAME=/index.php",
			"SCRIPT_FILENAME=/app/index.php",
			// php-cgi refuses to run without it (cgi.force_redirect).
			"REDIRECT_STATUS=200",
		},
		Stdout:    &stdout,
		Stderr:    os.Stderr,
		FS:        wazero.NewFSConfig().WithFSMount(scripts, "/app"),
		Functions: functions,
	})
	if err != nil || code != 0 {
		log.Printf("php-cgi: exit code %d: %v", code, err)
		http.Error(w, "PHP failed", http.StatusBadGateway)
		return
	}
	if err := writeCGI(w, &stdout); err != nil {
		log.Printf("php-cgi: %v", err)
	}
}

// writeCGI sends php-cgi's output: headers, a blank line, then the body.
func writeCGI(w http.ResponseWriter, out io.Reader) error {
	br := bufio.NewReader(out)
	header, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil {
		http.Error(w, "bad CGI response", http.StatusBadGateway)
		return err
	}
	status := http.StatusOK
	if s := header.Get("Status"); s != "" {
		if status, err = strconv.Atoi(strings.Fields(s)[0]); err != nil {
			http.Error(w, "bad CGI status", http.StatusBadGateway)
			return err
		}
		header.Del("Status")
	}
	for k, vs := range header {
		w.Header()[k] = vs
	}
	w.WriteHeader(status)
	_, err = io.Copy(w, br)
	return err
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *listen); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, listen string) (err error) {
	engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close(context.Background())) }()
	// Compile php-cgi.wasm now, so the first request does not wait for it.
	if err := engine.Compile(ctx); err != nil {
		return err
	}
	return serve(ctx, listen, handler{engine: engine})
}

// serve runs an HTTP server until ctx is done, then shuts it down.
func serve(ctx context.Context, listen string, h http.Handler) error {
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("listening on http://%s/?n=27", listen)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
