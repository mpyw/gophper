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

	"github.com/mpyw/gophper/internal/hostnet"
)

// engineABIVersion is the phpwasm.ABIVersion this host implements.
const engineABIVersion = 1

// Engine compiles the PHP binaries once and runs them many times.
// It is safe for concurrent use. Each run gets a fresh PHP instance.
type Engine struct {
	runtime wazero.Runtime
	cache   wazero.CompilationCache
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
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	e := &Engine{}
	if cfg.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cfg.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("compilation cache: %w", err)
		}
		e.cache = cache
		rc = rc.WithCompilationCache(cache)
	}
	e.runtime = wazero.NewRuntimeWithConfig(ctx, rc)
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
	inst := newEngineInstance(ctx)
	defer inst.sockets.Close()

	fs := opts.FS
	if fs == nil {
		fs = wazero.NewFSConfig()
	}
	fs = fs.WithFSMount(hostnet.SocketPlaceholderFS, hostnet.SocketPlaceholderDir)

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
	// Canceling stops the script the same way a timeout does.
	defer context.AfterFunc(ctx, inst.interrupt)()

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

// EngineConfig configures an Engine.
type EngineConfig struct {
	// CacheDir stores compiled machine code between processes. Empty disables the cache.
	CacheDir string
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
