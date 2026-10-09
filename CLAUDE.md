# gophper

The Go host that runs PHP from gophper-wasm on wazero. See README.md for the packages.

## Rules

- The PHP binaries live in github.com/mpyw/gophper-wasm. Its ABI.md is the contract with `engine.go`, `instance.go` and `internal/hostnet`.
- After an ABI change there, bump `engineABIVersion` in `engine.go` to match `phpwasm.ABIVersion`.
- To work on both at once, point `go.mod` at the local checkout with `replace github.com/mpyw/gophper-wasm => ../gophper-wasm`. `caddy/go.mod` needs its own `replace`, since a dependency's `replace` is ignored.
- `caddy/` is a separate module, so that the core does not depend on Caddy.

## Before you finish

```sh
go vet ./... && go test ./...
(cd caddy && go vet ./... && go test ./...)
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
| Ignoring the wasm binaries in Git | `go build` would need wasi-sdk, bison and re2c. They are committed in gophper-wasm instead. |
