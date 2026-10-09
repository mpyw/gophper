---
name: wasm-host
description: Change gophper's core - the Engine, instances, timeouts and interrupts, signals, host paths, processes, sockets, Go functions, or the dynamic linker for extensions (internal/dylink). Use before editing engine.go, instance.go, options.go, function.go, internal/wasi, internal/host* or internal/dylink.
---

# The wasm host

The root package is the public API and the wiring. Everything else is in `internal/`:

| Group | Package | Holds |
| --- | --- | --- |
| Public API | root: `engine.go`, `options.go`, `function.go` | `Engine`, `Options`, `EngineConfig`, `Function` |
| Wiring | root: `instance.go` | One run's state from each package below, handed to the host functions |
| Shared contract | `internal/wasi` | WASI's errno values, and `Run`: what a host function needs from the instance |
| ABI host functions | `internal/hostvm` | Linear memory, the interrupt flags, max_execution_time, `nanosleep`. `*hostvm.VM` is the `wasi.Run` |
| | `internal/hostsig` | Signals and `alarm` |
| | `internal/hostnet` | Sockets, pipes, DNS |
| | `internal/hostproc` | Child processes |
| | `internal/hostsys` | Users, locks, permissions, host paths |
| | `internal/hostfn` | PHP functions written in Go |
| Extensions | `internal/dylink`, `internal/dylink/wasmbin` | Links a PIC side module (`dl()`, `extension=`) against php.wasm's exports |
| Protocol | `internal/fcgi` | FastCGI, for `server` and the workers |

`internal/` stays flat. A package's name is its path's last element, so `internal/host/net` would be a package `net` that hides the standard library's.

## Rules

- Off by default: a library user who passes nothing gets a sandbox. The CLI and `server` turn host access on.
- A blocking host call must wake on an interrupt (`EINTR`) and give up once the run is over (`EIO`).
- PHP must never block on a writer the host holds. Pipes and stdout keep draining.

## Rejected designs

| Idea | Why not |
| --- | --- |
| Counting on Go to preempt an instance's goroutine | Go never preempts wazero's machine code. A busy PHP loop kept its P, so the timer it armed never ran there, and CI hung on `for (;;) {}`. The guest calls `yield` (gophper-wasm's `patches/0014`), which runs `runtime.Gosched`. `TestTimeoutOnOneProcessor` pins GOMAXPROCS to 1, where no other P can steal the timer. |
| `WithCloseOnContextDone(true)` for timeouts | Its checks made PHP 3.8 times slower (bench.php: 650 ms to 2.5 s). php-src's timer goes to Go, which sets `EG(vm_interrupt)`. |
| Writing the interrupt flags through `api.Memory` from the timer goroutine | Races with `memory.grow`, which replaces the buffer, so a flag could be lost. `hostvm`'s memory allocates the linear memory and serializes both. |
| A Go-side `--timeout` separate from `max_execution_time` | It disagreed with `ini_get()` and ignored `set_time_limit()`. |
| Canceling a context to wake `usleep()` | The cancel is permanent, so every later sleep returned at once. An interrupt closes a channel that is then replaced, like one signal. |
| `EINTR` for every blocking socket call after the run is over | PHP retries `EINTR` in its socket wait loop, so a canceled run spun forever. `hostnet` returns `EIO` once the context is done. |
| A Go dlopen that links PIC side modules into a PIE php.wasm | php.wasm stays a static executable, so its own code pays nothing for dynamic linking. It exports every symbol instead, and `internal/dylink` generates glue modules (`env`, `GOT.mem`, `GOT.func`). |
| Adding wasm definitions while imports are still being added | Imports are numbered first, so a definition's index changed with each later import. `dylink` adds every import, then the definitions. |
| Resolving a side module's function imports only against php.wasm | A weak function the module defines is imported too, so that another module could replace it. ICU's inline functions are. Such an import goes through a trampoline into the module's own export, set after it is instantiated. |
| `llvm-strip` without flags on an extension | It removes the `dylink.0` custom section, and the module then fails with out-of-bounds accesses. Strip with `--strip-debug`. `internal/dylink` rejects a module without `dylink.0`. |
| Resolving a side module's `read`, `poll` and the rest to php.wasm's exports of those names | Those are the libc originals. php.wasm links them with `--wrap`, so a side module must get the `__wrap_` export, or socket fds break. |
| One interrupt on cancel | `php_request_startup()` clears the flags, so a cancel before it was lost and a busy loop ran forever. `VM.InterruptUntil` raises them again until the run ends. |
| A symlink to gophper as `PHP_BINARY` | PHP resolves `PHP_BINARY` with `realpath`, which drops the name `php` that selects the subcommand. `cmd/gophper/phpbinary.go` writes a shell script instead. |
| Giving a child the instance's stdout as is | In `serve`, it is the HTTP response, and a child may outlive the request. `hostproc` hands children a writer that drops output once the run is over. |
| Host paths and processes on by default in `Options` | A library user who mounts only `/app` expects a sandbox. `HostPath` and `Processes` are off unless set, and the CLI and `server` set them. |
| Interrupting the VM with `VM.Interrupt` for a signal | It sets `EG(timed_out)` too, so a fatal "Maximum execution time" appeared. `VM.Wake` sets only `EG(vm_interrupt)`. A fatal signal is still handled by the guest, which exits quietly with 128 plus the number. |
| Letting PHP signal any host process | `proc_kill` reaches only the instance's own children. A script cannot kill gophper or other processes. |
| Naming a Unix socket by `hostpath.Guest` of its host path | A mount can put a path elsewhere, as `server` does with `/tmp`. `stream_socket_get_name` then showed the host path. `Sockets` keeps the path PHP gave for each host path. |
| Accepting any connection for a Windows `socketpair` | Another local process could connect first and take one end. `socketPairAccept` keeps only the connection from the dialer's own address. |
| Leaving the timers of a run to fire on their own | A hard timeout could re-arm after `SetTimeout(0)`, and a signal's grace timer outlived the run by 5 seconds. `VM.Stop` and `Signals.Stop` end them with the run. |
| Resolving a relative Unix socket path on the host | The host has no working directory of the guest's: `chdir()` happens in wasi-libc. gophper-wasm's `gophper_net.c` makes the path absolute, as `gophper_abs` does for files. |
| Passing php-src's `shutdown(fd, 1)` through as it is | 1 is `SHUT_WR` on POSIX, but `SHUT_RD` in WASI. php-cgi then closed with the web server's last `FCGI_STDIN` unread. Linux reset the connection, which lost the response. About 1 in 60 requests to a worker got a 502. gophper-wasm's `patches/0015` passes `SHUT_WR`. |
