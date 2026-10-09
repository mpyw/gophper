# gophper

The Go host that runs PHP from gophper-wasm on wazero. See README.md for the packages.

## Rules

- The PHP binaries live in github.com/mpyw/gophper-wasm. Its ABI.md is the contract with `engine.go`, `instance.go` and `internal/hostnet`.
- After an ABI change there, bump `engineABIVersion` in `engine.go` to match `phpwasm.ABIVersion`.
- To work on both at once, point `go.mod` at the local checkout with `replace github.com/mpyw/gophper-wasm => ../gophper-wasm`, or use a `go.work`.
- After `go.mod` changes, run `go generate ./cmd/gophper`. It collects the license files of the linked modules for `gophper licenses`, and a test fails without it.
- Only `cmd/gophper` and `caddy/` may import Caddy. depguard enforces it, so a program that imports the core links no Caddy code.

## Before you finish

```sh
go vet ./... && go test ./...
mise exec -- declscope shrink ./...
mise exec -- declscope ./...
```

declscope runs with `.declscope.yaml` (`qualify: ondemand`, `exported: true`). Read `.agents/skills/declscope-authoring` before adding or moving a declaration. `.claude` is a symlink to `.agents`.

`go test -race` is slow because wazero compiles under the race detector. Run it on a few tests at a time, with `-timeout 40m`.

## Rejected designs

| Idea | Why not |
| --- | --- |
| Write the interpreter from scratch in Go | The standard library is far larger than the language core. |
| `WithCloseOnContextDone(true)` for timeouts | Its checks made PHP 3.8 times slower (bench.php: 650 ms to 2.5 s). php-src's timer goes to Go, which sets `EG(vm_interrupt)`. |
| Writing the interrupt flags through `api.Memory` from the timer goroutine | Races with `memory.grow`, which replaces the buffer, so a flag could be lost. `engineMemory` allocates the linear memory and serializes both. |
| A Go-side `--timeout` separate from `max_execution_time` | It disagreed with `ini_get()` and ignored `set_time_limit()`. |
| Environment variables for php-cgi (`PHPRC`, `TMPDIR`, `REDIRECT_STATUS`) | Scripts saw them. The php.ini at `/etc/gophper` replaces all three. |
| Canceling a context to wake `usleep()` | The cancel is permanent, so every later sleep returned at once. An interrupt closes a channel that is then replaced, like one signal. |
| `EINTR` for every blocking socket call after the run is over | PHP retries `EINTR` in its socket wait loop, so a canceled run spun forever. `hostnet` returns `EIO` once the context is done. |
| `net/http/fcgi` for the FastCGI server | It drops `SCRIPT_NAME` and `PATH_INFO`. `internal/fcgi` passes every param through. |
| php-cgi's own FastCGI mode | Needs a listening socket on fd 0, and one instance serves one request at a time. php-cgi runs in plain CGI mode, one instance per request. |
| A Go dlopen that links PIC side modules into a PIE php.wasm | php.wasm stays a static executable, so its own code pays nothing for dynamic linking. It exports every symbol instead, and `internal/dylink` generates glue modules (`env`, `GOT.mem`, `GOT.func`). |
| Adding wasm definitions while imports are still being added | Imports are numbered first, so a definition's index changed with each later import. `dylink` adds every import, then the definitions. |
| Resolving a side module's function imports only against php.wasm | A weak function the module defines is imported too, so that another module could replace it. ICU's inline functions are. Such an import goes through a trampoline into the module's own export, set after it is instantiated. |
| `llvm-strip` without flags on an extension | It removes the `dylink.0` custom section, and the module then fails with out-of-bounds accesses. Strip with `--strip-debug`. `internal/dylink` rejects a module without `dylink.0`. |
| Resolving a side module's `read`, `poll` and the rest to php.wasm's exports of those names | Those are the libc originals. php.wasm links them with `--wrap`, so a side module must get the `__wrap_` export, or socket fds break. |
| One interrupt on cancel | `php_request_startup()` clears the flags, so a cancel before it was lost and a busy loop ran forever. `interruptUntil` raises them again until the run ends. |
| One `--root` for both the document root and what PHP may access | Laravel's `public/index.php` reads `../vendor`. `--root` is only the document root, and `--mount` (default: the current directory) is what PHP reaches. |
| Serving every existing file | `.env` and `.git` leaked when the root was a project directory. Any path segment starting with `.` is 404, except `.well-known`, as in Laravel's nginx config. |
| Running a router as `SCRIPT_FILENAME` | php-cgi cannot report `return false`, and `$_SERVER` would describe the router. `server/bootstrap/router.php` runs it and restores what php -S would set. |
| Passing router details in variables and unsetting them | With PHP's built-in `variables_order=EGPCS`, `getenv()` returns a copy of `$_ENV` taken at startup, so `putenv()` cannot hide them. The default php.ini sets `GPCS`, as `php.ini-production` does. |
| FastCGI running any `SCRIPT_FILENAME` | A web server could pass an uploaded file. `LimitExtensions` defaults to `.php` and `.phar`, like php-fpm's `security.limit_extensions`. |
| Ignoring the wasm binaries in Git | `go build` would need wasi-sdk, bison and re2c. They are committed in gophper-wasm instead. |
| A symlink to gophper as `PHP_BINARY` | PHP resolves `PHP_BINARY` with `realpath`, which drops the name `php` that selects the subcommand. `cmd/gophper/phpbinary.go` writes a shell script instead. |
| Giving a child the instance's stdout as is | In `serve`, it is the HTTP response, and a child may outlive the request. `hostproc` hands children a writer that drops output once the run is over. |
| Host paths and processes on by default in `Options` | A library user who mounts only `/app` expects a sandbox. `HostPath` and `Processes` are off unless set, and the CLI and `server` set them. |
| Mapping guest paths to host paths by identity in `server` | `/tmp` is `TempDir` there, and a read-only mount must refuse `chmod`. `pool.hostPath` follows the mounts. |
| Leaving `HOME` unset for a nested `gophper php` | Caddy, linked in, warns at startup without a config directory. The `PHP_BINARY` script sets `XDG_CONFIG_HOME`, and `gophper php` hides it from PHP again. |
| A path PHP writes and reads again, outside `HostPath` | `stat` then reports uid 0 and mode 0. opcache rejects a file cache entry it does not own, so it compiled every script on every request, and `serve` got two times slower. `pool.hostPath` covers the opcache directory. |
| Keying opcache's file cache by opcache's system id alone | Two builds of one PHP release share it. `Engine.BuildID` hashes the binaries, and the pool's cache directory goes under it. |
| A fresh PHP instance for every request | Starting one takes 12 ms, more than most requests. Workers run php-cgi in FastCGI mode on a Unix socket, as php-fpm's children do, and `internal/fcgi.Do` sends them requests. `NoWorkers` keeps the old way. |
| php-cgi's own `PHP_FCGI_MAX_REQUESTS` | It closed the socket under a request that had already connected, which got a 502. The pool counts requests, sets `PHP_FCGI_MAX_REQUESTS=0`, and stops a worker itself. |
| Workers with opcache's file cache only | Laravel took 84 ms, and 23 ms with shared memory. An earlier try with shared memory was slower only because php.wasm had no SHM backend, and opcache recompiled every script. Shared memory is per worker, so `poolOpcacheWorkerINI` sets 64 MB: 128 MB put 4 workers at 1.96 GB RSS. |
| Interrupting the VM with `interrupt()` for a signal | It sets `EG(timed_out)` too, so a fatal "Maximum execution time" appeared. `wake()` sets only `EG(vm_interrupt)`. A fatal signal is still handled by the guest, which exits quietly with 128 plus the number. |
| Letting PHP signal any host process | `proc_kill` reaches only the instance's own children. A script cannot kill gophper or other processes. |
| `caddy/` as a separate Go module with its own `gophper-caddy` binary | Two binaries to install, and two `replace` lines while developing. The Go linker drops packages nobody imports, so one module costs the core nothing. |
