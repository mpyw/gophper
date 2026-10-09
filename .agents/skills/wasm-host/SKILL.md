---
name: wasm-host
description: Change gophper's core - the Engine, instances, timeouts and interrupts, signals, host paths, processes, sockets, Go functions, or the dynamic linker for extensions (internal/dylink). Use before editing engine.go, instance.go, options.go, signal.go, function.go, or internal/host*, internal/dylink.
---

# The wasm host

| Piece | Where |
| --- | --- |
| Compile once, instantiate per run | `engine.go`, `instance.go` |
| What a run may reach | `options.go`: `FS`, `HostPath`, `Processes`, `Functions`, `Signals` |
| Timer and interrupts | `engine.go`. php-src's timer calls the host, which sets the interrupt flags. |
| Extensions (`dl()`, `extension=`) | `internal/dylink`: links a PIC side module against php.wasm's exports |
| Host functions | `internal/host*` (gophper-wasm-upgrade skill) |

## Rules

- Off by default: a library user who passes nothing gets a sandbox. The CLI and `server` turn host access on.
- A blocking host call must wake on an interrupt (`EINTR`) and give up once the run is over (`EIO`).
- PHP must never block on a writer the host holds. Pipes and stdout keep draining.

## Rejected designs

| Idea | Why not |
| --- | --- |
| `WithCloseOnContextDone(true)` for timeouts | Its checks made PHP 3.8 times slower (bench.php: 650 ms to 2.5 s). php-src's timer goes to Go, which sets `EG(vm_interrupt)`. |
| Writing the interrupt flags through `api.Memory` from the timer goroutine | Races with `memory.grow`, which replaces the buffer, so a flag could be lost. `engineMemory` allocates the linear memory and serializes both. |
| A Go-side `--timeout` separate from `max_execution_time` | It disagreed with `ini_get()` and ignored `set_time_limit()`. |
| Canceling a context to wake `usleep()` | The cancel is permanent, so every later sleep returned at once. An interrupt closes a channel that is then replaced, like one signal. |
| `EINTR` for every blocking socket call after the run is over | PHP retries `EINTR` in its socket wait loop, so a canceled run spun forever. `hostnet` returns `EIO` once the context is done. |
| A Go dlopen that links PIC side modules into a PIE php.wasm | php.wasm stays a static executable, so its own code pays nothing for dynamic linking. It exports every symbol instead, and `internal/dylink` generates glue modules (`env`, `GOT.mem`, `GOT.func`). |
| Adding wasm definitions while imports are still being added | Imports are numbered first, so a definition's index changed with each later import. `dylink` adds every import, then the definitions. |
| Resolving a side module's function imports only against php.wasm | A weak function the module defines is imported too, so that another module could replace it. ICU's inline functions are. Such an import goes through a trampoline into the module's own export, set after it is instantiated. |
| `llvm-strip` without flags on an extension | It removes the `dylink.0` custom section, and the module then fails with out-of-bounds accesses. Strip with `--strip-debug`. `internal/dylink` rejects a module without `dylink.0`. |
| Resolving a side module's `read`, `poll` and the rest to php.wasm's exports of those names | Those are the libc originals. php.wasm links them with `--wrap`, so a side module must get the `__wrap_` export, or socket fds break. |
| One interrupt on cancel | `php_request_startup()` clears the flags, so a cancel before it was lost and a busy loop ran forever. `interruptUntil` raises them again until the run ends. |
| A symlink to gophper as `PHP_BINARY` | PHP resolves `PHP_BINARY` with `realpath`, which drops the name `php` that selects the subcommand. `cmd/gophper/phpbinary.go` writes a shell script instead. |
| Giving a child the instance's stdout as is | In `serve`, it is the HTTP response, and a child may outlive the request. `hostproc` hands children a writer that drops output once the run is over. |
| Host paths and processes on by default in `Options` | A library user who mounts only `/app` expects a sandbox. `HostPath` and `Processes` are off unless set, and the CLI and `server` set them. |
| Interrupting the VM with `interrupt()` for a signal | It sets `EG(timed_out)` too, so a fatal "Maximum execution time" appeared. `wake()` sets only `EG(vm_interrupt)`. A fatal signal is still handled by the guest, which exits quietly with 128 plus the number. |
| Letting PHP signal any host process | `proc_kill` reaches only the instance's own children. A script cannot kill gophper or other processes. |
