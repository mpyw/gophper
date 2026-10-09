// Command gophper runs PHP on wazero.
//
//	gophper php [php options] [file] [args...]   the php CLI
//	gophper serve [options]                      an HTTP(S) server for a PHP app
//	gophper fcgi [options]                       a FastCGI server, like php-fpm
//	gophper caddy [caddy command]                Caddy, with gophper's module built in
//
// Every option can also come from an environment variable: GOPHPER_ and
// the option's name in upper snake case, such as GOPHPER_MAX_WAIT_TIME.
// Invoked under the name "php" (through a symlink, for example), it behaves
// like "gophper php".
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	phpwasm "github.com/mpyw/gophper-wasm"
	phpext "github.com/mpyw/gophper-wasm/ext"
	"github.com/tetratelabs/wazero"
	"github.com/urfave/cli/v3"
	"golang.org/x/crypto/acme/autocert"

	"github.com/mpyw/gophper"
	gophpercaddy "github.com/mpyw/gophper/caddy"
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
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "extension-dir",
				Usage:   "directory of extensions (.so wasm side modules) that extension= and dl() load",
				Sources: env("EXTENSION_DIR"),
			},
			&cli.StringFlag{
				Name:    "cache-dir",
				Value:   gophper.DefaultEngineConfig().CacheDir,
				Usage:   "where compiled code is kept between runs",
				Sources: env("CACHE_DIR"),
			},
			&cli.BoolFlag{
				Name:    "no-cache",
				Usage:   "compile the PHP binaries on every start",
				Sources: env("NO_CACHE"),
			},
		},
		Commands: []*cli.Command{
			{
				Name:      "php",
				Aliases:   []string{"cli"},
				Usage:     "run the php CLI; every argument goes to PHP",
				ArgsUsage: "[php options] [file] [args...]",
				Description: "The host file system is mounted at /, and PHP starts in the current directory.\n" +
					"php -S runs PHP's built-in development server, in a single instance.",
				// "-r", "-d" and the rest belong to PHP, not to gophper.
				SkipFlagParsing: true,
				Action:          phpAction,
			},
			{
				Name:  "serve",
				Usage: "serve a PHP app over HTTP or HTTPS, with no web server in front",
				Description: "Without --router: .php files run, other files are sent as they are,\n" +
					"directories run their index file, and every other path runs the front controller.\n" +
					"Paths with a segment that starts with \".\" are not found, except /.well-known.",
				Flags:  append(serveFlags(), phpFlags()...),
				Action: serveAction,
			},
			{
				Name:      "caddy",
				Usage:     "run Caddy, with the gophper module and directive built in",
				ArgsUsage: "[caddy command] [options]",
				Description: "Every argument goes to Caddy. Try \"gophper caddy php-server\" or\n" +
					"\"gophper caddy run --config Caddyfile\". See \"gophper caddy help\".",
				SkipFlagParsing: true,
				Action:          caddyAction,
			},
			{
				Name:   "licenses",
				Usage:  "print the licenses of gophper and everything it contains",
				Action: licensesAction,
			},
			{
				Name:  "extension",
				Usage: "manage the extensions that come with gophper",
				Commands: []*cli.Command{
					{
						Name:   "list",
						Usage:  "list the extensions that come with gophper",
						Action: extensionListAction,
					},
					{
						Name:      "install",
						Usage:     "write extensions to --extension-dir as <name>.so",
						ArgsUsage: "name...",
						Description: "Then load one with extension=<name>, as in\n" +
							"\"gophper --extension-dir DIR php -d extension=<name>\".",
						Action: extensionInstallAction,
					},
				},
			},
			{
				Name:  "fcgi",
				Usage: "serve PHP over FastCGI, like php-fpm",
				Description: "The web server chooses the script through SCRIPT_FILENAME, which must be\n" +
					"inside a --mount and end in one of --limit-extensions.",
				Flags:  append(fcgiFlags(), phpFlags()...),
				Action: fcgiAction,
			},
		},
	}
}

// env names an option's environment variable.
func env(name string) cli.ValueSourceChain {
	return cli.EnvVars("GOPHPER_" + name)
}

// phpFlags are the options serve and fcgi share: how PHP runs.
func phpFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "mount",
			Usage:   "directory PHP may access, at the same path inside PHP; DIR or DIR:ro (repeatable, default: the current directory)",
			Sources: env("MOUNT"),
		},
		&cli.BoolFlag{
			Name:    "no-workers",
			Usage:   "start a fresh PHP instance for each request, instead of reusing workers as php-fpm does",
			Sources: env("NO_WORKERS"),
		},
		&cli.IntFlag{
			Name:    "max-requests",
			Usage:   "requests a worker serves before it is replaced, like php-fpm's pm.max_requests (0 = 500)",
			Sources: env("MAX_REQUESTS"),
		},
		&cli.StringFlag{
			Name:    "opcache-dir",
			Usage:   "where opcache keeps compiled scripts between requests (default: the user cache directory)",
			Sources: env("OPCACHE_DIR"),
		},
		&cli.BoolFlag{
			Name:    "no-opcache",
			Usage:   "leave opcache off",
			Sources: env("NO_OPCACHE"),
		},
		&cli.BoolFlag{
			Name:    "no-processes",
			Usage:   "stop PHP from starting host programs (proc_open, exec and the rest)",
			Sources: env("NO_PROCESSES"),
		},
		&cli.StringFlag{
			Name:    "temp-dir",
			Usage:   "directory mounted at /tmp inside PHP (default: the system's)",
			Sources: env("TEMP_DIR"),
		},
		&cli.IntFlag{
			Name:    "concurrency",
			Usage:   "max PHP instances running at once, like php-fpm's pm.max_children (0 = number of CPUs)",
			Sources: env("CONCURRENCY"),
		},
		&cli.DurationFlag{
			Name:    "max-wait-time",
			Usage:   "how long a request waits for a free instance before 503 (0 = no limit)",
			Sources: env("MAX_WAIT_TIME"),
		},
		&cli.StringFlag{
			Name:    "php-ini",
			Aliases: []string{"c"},
			Usage:   "php.ini file to load; -d entries come after it",
			Sources: env("PHP_INI"),
		},
		&cli.StringSliceFlag{
			Name:      "define",
			Aliases:   []string{"d"},
			Usage:     "php.ini entry as key=value, such as max_execution_time=30 (repeatable)",
			Sources:   env("DEFINE"),
			Validator: keyValues("define"),
		},
		&cli.StringSliceFlag{
			Name:      "env",
			Aliases:   []string{"e"},
			Usage:     "environment variable for PHP as KEY=VALUE; the host's are not passed (repeatable)",
			Sources:   env("ENV"),
			Validator: keyValues("env"),
		},
		&cli.StringFlag{
			Name:    "access-log",
			Usage:   `file to append an access log to, or "-" for stdout`,
			Sources: env("ACCESS_LOG"),
		},
	}
}

func serveFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "listen",
			Aliases: []string{"l"},
			Usage:   "address to listen on (default: 127.0.0.1:8080, or :443 with --domain)",
			Sources: env("LISTEN"),
		},
		&cli.StringFlag{
			Name:    "root",
			Aliases: []string{"r"},
			Value:   ".",
			Usage:   "document root; it must be inside a --mount",
			Sources: env("ROOT"),
		},
		&cli.StringFlag{
			Name:    "router",
			Usage:   "script that runs for every request, as with php -S; returning false sends the file as is",
			Sources: env("ROUTER"),
		},
		&cli.StringSliceFlag{
			Name:    "index",
			Usage:   "file a directory runs (repeatable, default: index.php)",
			Sources: env("INDEX"),
		},
		&cli.StringFlag{
			Name:    "front-controller",
			Value:   "index.php",
			Usage:   `script for paths that are not files, or "off" for 404`,
			Sources: env("FRONT_CONTROLLER"),
		},
		&cli.StringSliceFlag{
			Name:    "split-path",
			Usage:   "suffix that ends a script's path, before PATH_INFO (repeatable, default: .php)",
			Sources: env("SPLIT_PATH"),
		},
		&cli.BoolFlag{
			Name:    "no-static",
			Usage:   "answer files other than scripts with 404",
			Sources: env("NO_STATIC"),
		},
		&cli.StringFlag{
			Name:    "max-body",
			Value:   "64M",
			Usage:   `request body limit, such as 64M or 1G ("0" = no limit)`,
			Sources: env("MAX_BODY"),
		},
		&cli.StringFlag{
			Name:    "domain",
			Usage:   "domain to serve with a Let's Encrypt certificate; also listens on :80 for the challenge",
			Sources: env("DOMAIN"),
		},
		&cli.StringFlag{
			Name:    "tls-cert",
			Usage:   "certificate file; serves HTTPS with --tls-key",
			Sources: env("TLS_CERT"),
		},
		&cli.StringFlag{
			Name:    "tls-key",
			Usage:   "private key file for --tls-cert",
			Sources: env("TLS_KEY"),
		},
	}
}

func fcgiFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "listen",
			Aliases: []string{"l"},
			Value:   "127.0.0.1:9000",
			Usage:   `TCP address, or "unix:/path/to.sock"`,
			Sources: env("LISTEN"),
		},
		&cli.StringFlag{
			Name:    "listen-mode",
			Value:   "0660",
			Usage:   "permissions of a Unix socket, like php-fpm's listen.mode",
			Sources: env("LISTEN_MODE"),
		},
		&cli.StringSliceFlag{
			Name:    "allowed-clients",
			Usage:   "address or prefix that may connect over TCP, like php-fpm's listen.allowed_clients (repeatable, default: any)",
			Sources: env("ALLOWED_CLIENTS"),
		},
		&cli.StringSliceFlag{
			Name:    "limit-extensions",
			Usage:   "extension a script may have, like php-fpm's security.limit_extensions (repeatable, default: .php .phar)",
			Sources: env("LIMIT_EXTENSIONS"),
		},
		&cli.StringFlag{
			Name:    "ping-path",
			Usage:   `path that answers "pong", like php-fpm's ping.path`,
			Sources: env("PING_PATH"),
		},
		&cli.StringFlag{
			Name:    "status-path",
			Usage:   "path that answers counters, like php-fpm's pm.status_path",
			Sources: env("STATUS_PATH"),
		},
	}
}

func keyValues(flag string) func([]string) error {
	return func(entries []string) error {
		for _, e := range entries {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("--%s: want key=value, got %q", flag, e)
			}
		}
		return nil
	}
}

func newEngine(ctx context.Context, cmd *cli.Command) (*gophper.Engine, error) {
	cfg, err := engineConfig(cmd)
	if err != nil {
		return nil, err
	}
	return gophper.NewEngine(ctx, cfg)
}

// engineConfig reads the global options.
func engineConfig(cmd *cli.Command) (gophper.EngineConfig, error) {
	cfg := gophper.EngineConfig{CacheDir: cmd.String("cache-dir")}
	// The global options again, for PHP_BINARY's script.
	var global []string
	if cmd.Bool("no-cache") {
		cfg.CacheDir = ""
		global = append(global, "--no-cache")
	} else if cfg.CacheDir != "" {
		abs, err := filepath.Abs(cfg.CacheDir)
		if err != nil {
			return gophper.EngineConfig{}, err
		}
		cfg.CacheDir = abs
		global = append(global, "--cache-dir", abs)
	}
	if dir := cmd.String("extension-dir"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return gophper.EngineConfig{}, err
		}
		cfg.ExtensionDir = abs
		global = append(global, "--extension-dir", abs)
	}
	bin, err := phpBinaryScript(cfg.CacheDir, global)
	if err != nil {
		// PHP still runs. Only PHP_BINARY is empty.
		fmt.Fprintf(cmd.Root().ErrWriter, "gophper: PHP_BINARY: %v\n", err)
	}
	cfg.PHPBinary = bin
	return cfg, nil
}

// phpConfig reads the options in phpFlags. The returned function closes
// the access log.
func phpConfig(cmd *cli.Command) (server.PHPConfig, func(), error) {
	cfg := server.PHPConfig{
		TempDir:     cmd.String("temp-dir"),
		NoProcesses: cmd.Bool("no-processes"),
		OpcacheDir:  cmd.String("opcache-dir"),
		NoWorkers:   cmd.Bool("no-workers"),
		MaxRequests: int(cmd.Int("max-requests")),
		NoOpcache:   cmd.Bool("no-opcache"),
		Concurrency: int(cmd.Int("concurrency")),
		MaxWaitTime: cmd.Duration("max-wait-time"),
		Env:         cmd.StringSlice("env"),
	}
	mounts := cmd.StringSlice("mount")
	if len(mounts) == 0 {
		mounts = []string{"."}
	}
	for _, m := range mounts {
		dir, ro := strings.CutSuffix(m, ":ro")
		abs, err := filepath.Abs(dir)
		if err != nil {
			return cfg, nil, err
		}
		cfg.Mounts = append(cfg.Mounts, server.Mount{Dir: abs, ReadOnly: ro})
	}
	if file := cmd.String("php-ini"); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return cfg, nil, err
		}
		cfg.INI = strings.Split(string(b), "\n")
	}
	cfg.INI = append(cfg.INI, cmd.StringSlice("define")...)

	closeLog := func() {}
	switch path := cmd.String("access-log"); path {
	case "":
	case "-":
		cfg.AccessLog = os.Stdout
	default:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return cfg, nil, err
		}
		cfg.AccessLog = f
		closeLog = func() { f.Close() }
	}
	return cfg, closeLog, nil
}

// phpAction mounts the host file system at "/" and starts in the current directory.
func phpAction(ctx context.Context, cmd *cli.Command) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	engine, err := newEngine(ctx, cmd)
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())
	// Signals go to PHP, as to a native php: pcntl handlers run, and the
	// rest end the script as their default action would. So the run does
	// not stop with ctx, which main cancels on the first SIGINT.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, phpSignals...)
	defer signal.Stop(signals)
	code, err := engine.RunCLI(context.WithoutCancel(ctx), gophper.Options{
		Args:   cmd.Args().Slice(),
		Env:    phpBinaryEnv(os.Environ()),
		Dir:    wd,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		FS:     wazero.NewFSConfig().WithDirMount("/", "/"),
		// The host's root is mounted at "/", so paths are the same.
		HostPath:  func(path string) (string, bool, bool) { return path, true, true },
		Processes: true,
		Signals:   signals,
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
	cert, key, domain := cmd.String("tls-cert"), cmd.String("tls-key"), cmd.String("domain")
	switch {
	case (cert == "") != (key == ""):
		return errors.New("--tls-cert and --tls-key go together")
	case domain != "" && cert != "":
		return errors.New("--domain gets its own certificate; drop --tls-cert and --tls-key")
	}
	maxBody, err := parseSize(cmd.String("max-body"))
	if err != nil {
		return fmt.Errorf("--max-body: %w", err)
	}

	php, closeLog, err := phpConfig(cmd)
	if err != nil {
		return err
	}
	defer closeLog()
	cfg := server.HTTPConfig{
		PHPConfig:   php,
		Root:        cmd.String("root"),
		Router:      cmd.String("router"),
		Index:       cmd.StringSlice("index"),
		SplitPath:   cmd.StringSlice("split-path"),
		NoStatic:    cmd.Bool("no-static"),
		MaxBodySize: maxBody,
	}
	if fc := cmd.String("front-controller"); fc == "off" {
		cfg.NoFrontController = true
	} else {
		cfg.FrontController = fc
	}

	engine, err := newEngine(ctx, cmd)
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())
	h, err := server.NewHTTPHandler(engine, cfg)
	if err != nil {
		return err
	}
	defer h.Close()

	listen := cmd.String("listen")
	switch {
	case listen != "":
	case domain != "":
		listen = ":443"
	default:
		listen = "127.0.0.1:8080"
	}
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 30 * time.Second}
	var servers []*http.Server
	switch {
	case domain != "":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(domain),
			Cache:      autocert.DirCache(filepath.Join(gophper.DefaultEngineConfig().CacheDir, "autocert")),
		}
		srv.TLSConfig = m.TLSConfig()
		// :80 answers the ACME challenge and redirects everything else to HTTPS.
		servers = append(servers, &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 30 * time.Second})
	case cert != "":
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return err
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{pair}}
	}
	servers = append(servers, srv)

	errs := make(chan error, len(servers))
	for _, s := range servers {
		l, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		scheme := "http"
		if s.TLSConfig != nil {
			scheme = "https"
		}
		fmt.Fprintf(os.Stderr, "gophper: serving %s on %s://%s\n", cfg.Root, scheme, l.Addr())
		go func() {
			if s.TLSConfig != nil {
				errs <- s.ServeTLS(l, "", "")
			} else {
				errs <- s.Serve(l)
			}
		}()
	}

	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdownCtx)
	}
	return nil
}

func fcgiAction(ctx context.Context, cmd *cli.Command) error {
	php, closeLog, err := phpConfig(cmd)
	if err != nil {
		return err
	}
	defer closeLog()
	cfg := server.FastCGIConfig{
		PHPConfig:       php,
		LimitExtensions: cmd.StringSlice("limit-extensions"),
		AllowedClients:  cmd.StringSlice("allowed-clients"),
		PingPath:        cmd.String("ping-path"),
		StatusPath:      cmd.String("status-path"),
	}
	mode, err := strconv.ParseUint(cmd.String("listen-mode"), 8, 32)
	if err != nil {
		return fmt.Errorf("--listen-mode: %w", err)
	}

	engine, err := newEngine(ctx, cmd)
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())
	srv, err := server.NewFastCGIServer(engine, cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

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
	if network == "unix" {
		if err := os.Chmod(address, os.FileMode(mode)); err != nil {
			l.Close()
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "gophper: FastCGI on %s\n", listen)
	return srv.Serve(ctx, l)
}

// caddyAction hands the arguments to Caddy's own command line. It exits.
// licensesAction prints what a distributor of gophper must pass on.
func licensesAction(_ context.Context, cmd *cli.Command) error {
	w := cmd.Root().Writer
	fmt.Fprintf(w, "gophper\n\n%s\n", gophper.License)
	entries, err := fs.ReadDir(phpwasm.Licenses, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fs.ReadFile(phpwasm.Licenses, e.Name())
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n---- %s ----\n\n%s\n", strings.TrimSuffix(e.Name(), ".txt"), b)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Fprint(w, "\n---- Go modules ----\n\nEach is under the license in its source.\n\n")
		for _, m := range info.Deps {
			fmt.Fprintf(w, "%s %s\n", m.Path, m.Version)
		}
	}
	return nil
}

func extensionListAction(_ context.Context, cmd *cli.Command) error {
	for _, name := range phpext.Names() {
		fmt.Fprintln(cmd.Root().Writer, name)
	}
	return nil
}

func extensionInstallAction(_ context.Context, cmd *cli.Command) error {
	dir := cmd.Root().String("extension-dir")
	switch {
	case dir == "":
		return errors.New("extension install: set --extension-dir, before the subcommand")
	case cmd.Args().Len() == 0:
		return fmt.Errorf("extension install: name an extension (%s)", strings.Join(phpext.Names(), ", "))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range cmd.Args().Slice() {
		if !slices.Contains(phpext.Names(), name) {
			return fmt.Errorf("extension install: no extension %q (%s)", name, strings.Join(phpext.Names(), ", "))
		}
		so, err := phpext.Open(name)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name+".so")
		if err := os.WriteFile(path, so, 0o644); err != nil {
			return err
		}
		fmt.Fprintln(cmd.Root().Writer, path)
		// Such as intl's ICU data, which it reads from the same directory.
		for _, file := range phpext.Files(name) {
			b, err := phpext.OpenFile(name, file)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, file)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				return err
			}
			fmt.Fprintln(cmd.Root().Writer, path)
		}
	}
	return nil
}

func caddyAction(_ context.Context, cmd *cli.Command) error {
	cfg, err := engineConfig(cmd)
	if err != nil {
		return err
	}
	gophpercaddy.HandlerEngineConfig = func() gophper.EngineConfig { return cfg }
	os.Args = append([]string{"gophper caddy"}, cmd.Args().Slice()...)
	caddycmd.Main()
	return nil
}

// parseSize reads a byte count such as 64M or 1G. "0" means no limit: -1.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for suffix, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30} {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			s, mult = v, m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("want a size such as 64M, got %q", s)
	}
	if n == 0 {
		return -1, nil
	}
	return n * mult, nil
}
