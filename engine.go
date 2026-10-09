package gophper

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	phpwasm "github.com/mpyw/gophper-wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/mpyw/gophper/internal/dylink"
	"github.com/mpyw/gophper/internal/hostnet"
	"github.com/mpyw/gophper/internal/hostproc"
)

// engineABIVersion is the phpwasm.ABIVersion this host implements.
const engineABIVersion = 4

// Engine compiles the PHP binaries once and runs them many times.
// It is safe for concurrent use. Each run gets a fresh PHP instance.
type Engine struct {
	runtime wazero.Runtime
	cache   wazero.CompilationCache
	// dylink compiles extensions once for every instance.
	dylink       *dylink.Cache
	extensionDir string
	phpBinary    string
	// Each binary is compiled on first use, so a CLI run does not pay for php-cgi.
	cliModule func() (wazero.CompiledModule, error)
	cgiModule func() (wazero.CompiledModule, error)
}

// NewEngine prepares the runtime. The PHP binaries are compiled on first use.
func NewEngine(ctx context.Context, cfg EngineConfig) (*Engine, error) {
	if phpwasm.ABIVersion != engineABIVersion {
		return nil, fmt.Errorf("github.com/mpyw/gophper-wasm has ABI version %d, but this gophper implements %d; use matching versions", phpwasm.ABIVersion, engineABIVersion)
	}

	// WithCloseOnContextDone is left off: its checks made PHP 3.8 times slower.
	// Runs are stopped through PHP's own interrupt flags instead. See engineInstance.
	rc := wazero.NewRuntimeConfig().
		// Extended constant expressions place a side module's data at __memory_base.
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling | experimental.CoreFeaturesExtendedConst)
	e := &Engine{extensionDir: cfg.ExtensionDir, phpBinary: cfg.PHPBinary}
	if cfg.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cfg.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("compilation cache: %w", err)
		}
		e.cache = cache
		rc = rc.WithCompilationCache(cache)
	}
	e.runtime = wazero.NewRuntimeWithConfig(ctx, rc)
	e.dylink = dylink.NewCache(e.runtime)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, e.runtime); err != nil {
		e.Close(ctx)
		return nil, err
	}
	host := e.runtime.NewHostModuleBuilder("gophper")
	host.NewFunctionBuilder().WithFunc(engineSetTimeout).Export("set_timeout")
	hostnet.ExportSockets(host, func(ctx context.Context) *hostnet.Sockets {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return inst.sockets
		}
		return nil
	})
	dylink.Export(host, func(ctx context.Context) *dylink.Linker {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return inst.linker
		}
		return nil
	})
	hostproc.ExportProcesses(host, func(ctx context.Context) *hostproc.Processes {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return inst.processes
		}
		return nil
	})
	hostnet.ExportDNS(host, func(ctx context.Context) hostnet.Run {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return inst
		}
		return nil
	})
	if _, err := host.Instantiate(ctx); err != nil {
		e.Close(ctx)
		return nil, err
	}

	// Compilation must outlive the ctx of the first run that triggers it.
	compileCtx := context.WithoutCancel(ctx)
	compile := func(name string, bin func() ([]byte, error)) func() (wazero.CompiledModule, error) {
		return sync.OnceValues(func() (wazero.CompiledModule, error) {
			b, err := bin()
			if err != nil {
				return nil, fmt.Errorf("decompress %s: %w", name, err)
			}
			if err := e.dylink.AddMain(b); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			m, err := e.runtime.CompileModule(compileCtx, b)
			if err != nil {
				return nil, fmt.Errorf("compile %s: %w", name, err)
			}
			return m, nil
		})
	}
	e.cliModule = compile("php.wasm", phpwasm.CLI)
	e.cgiModule = compile("php-cgi.wasm", phpwasm.CGI)
	return e, nil
}

// Close releases the runtime and the compilation cache.
func (e *Engine) Close(ctx context.Context) error {
	err := e.runtime.Close(ctx)
	if e.cache != nil {
		err = errors.Join(err, e.cache.Close(ctx))
	}
	return err
}

// RunCLI runs the PHP CLI SAPI and returns its exit code.
func (e *Engine) RunCLI(ctx context.Context, opts Options) (int, error) {
	return e.run(ctx, e.cliModule, "php", opts)
}

// RunCGI runs php-cgi once in CGI mode and returns its exit code.
// The request comes from CGI variables in opts.Env and the body from opts.Stdin.
// The raw CGI response, headers included, goes to opts.Stdout.
func (e *Engine) RunCGI(ctx context.Context, opts Options) (int, error) {
	return e.run(ctx, e.cgiModule, "php-cgi", opts)
}

func (e *Engine) run(ctx context.Context, compiled func() (wazero.CompiledModule, error), argv0 string, opts Options) (int, error) {
	mod, err := compiled()
	if err != nil {
		return 0, err
	}
	inst := newEngineInstance(ctx, e.dylink, opts)
	defer inst.sockets.Close()
	defer inst.processes.Close()
	defer inst.linker.Close(context.WithoutCancel(ctx))

	fs := opts.FS
	if fs == nil {
		fs = wazero.NewFSConfig()
	}
	fs = fs.WithFSMount(hostnet.SocketPlaceholderFS, hostnet.SocketPlaceholderDir).
		// OPENSSLDIR: openssl.cnf and the CA bundle.
		WithFSMount(phpwasm.SSL, "/etc/gophper/ssl")
	if e.extensionDir != "" {
		fs = fs.WithReadOnlyDirMount(e.extensionDir, engineExtensionDir)
	}
	if e.phpBinary != "" {
		// PHP_BINARY is checked from inside, so its directory must be visible.
		dir := filepath.Dir(e.phpBinary)
		fs = fs.WithReadOnlyDirMount(dir, filepath.ToSlash(dir))
		argv0 = e.phpBinary
	}

	mc := wazero.NewModuleConfig().
		// _start is called below, once the interrupt flags are located.
		WithStartFunctions().
		// An empty name lets many instances run at the same time.
		WithName("").
		WithArgs(append([]string{argv0}, opts.Args...)...).
		WithSysWalltime().
		WithSysNanotime().
		WithNanosleep(inst.nanosleep).
		WithRandSource(engineRandSource{}).
		WithFSConfig(fs)
	if opts.Stdin != nil {
		mc = mc.WithStdin(opts.Stdin)
	}
	if opts.Stdout != nil {
		mc = mc.WithStdout(opts.Stdout)
	}
	if opts.Stderr != nil {
		mc = mc.WithStderr(opts.Stderr)
	}
	if opts.Dir != "" {
		// Read by a constructor in gophper-wasm's compat/, before main().
		mc = mc.WithEnv("GOPHPER_CWD", opts.Dir)
	}
	for _, kv := range opts.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			mc = mc.WithEnv(k, v)
		}
	}

	// The instance allocates the linear memory, so that an interrupt can
	// write to it while the module grows it.
	m, err := e.runtime.InstantiateModule(experimental.WithMemoryAllocator(ctx, inst), mod, mc)
	if err != nil {
		return 0, err
	}
	defer m.Close(context.WithoutCancel(ctx))
	if err := inst.locate(m); err != nil {
		return 0, err
	}
	defer inst.setTimeout(0)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Canceling stops the script the same way a timeout does.
	done := make(chan struct{})
	defer close(done)
	defer context.AfterFunc(ctx, func() { inst.interruptUntil(done) })()

	_, err = m.ExportedFunction("_start").Call(context.WithValue(ctx, engineInstanceKey{}, inst))
	code := 0
	if exitErr := (*sys.ExitError)(nil); errors.As(err, &exitErr) {
		code, err = int(exitErr.ExitCode()), nil
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return code, err
}

// engineExtensionDir is php.wasm's extension_dir (gophper-wasm's ABI.md).
const engineExtensionDir = "/usr/local/lib/php/extensions"

// EngineConfig configures an Engine.
type EngineConfig struct {
	// CacheDir stores compiled machine code between processes. Empty disables the cache.
	CacheDir string
	// ExtensionDir holds extensions built by gophper-wasm's scripts/build-ext.sh.
	// PHP loads them with extension=<name> or dl(). Empty means none.
	ExtensionDir string
	// PHPBinary is a host program that runs PHP, such as a script that
	// runs "gophper php". PHP_BINARY reports it, so that a script starting
	// PHP again, as Composer and Laravel do, runs it. Empty leaves
	// PHP_BINARY empty.
	PHPBinary string
}

// DefaultEngineConfig caches compiled code in a per-user directory.
func DefaultEngineConfig() EngineConfig {
	dir, err := os.UserCacheDir()
	if err != nil {
		return EngineConfig{}
	}
	return EngineConfig{CacheDir: filepath.Join(dir, "gophper")}
}

// engineRandSource feeds WASI random_get from the host CSPRNG.
type engineRandSource struct{}

func (engineRandSource) Read(p []byte) (int, error) { return rand.Read(p) }
