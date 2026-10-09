# gophper

The Go host that runs PHP from gophper-wasm on wazero. See README.md for the packages.

## Rules

- The PHP binaries live in github.com/mpyw/gophper-wasm. Its ABI.md is the contract with `engine.go` and `internal/host*`.
- `engineABIVersion` in `engine.go` must equal `phpwasm.ABIVersion`.
- gophper-wasm's branches hold no binaries. Test against a local checkout only after building it (`gophper-wasm-upgrade` skill).
- After `go.mod` changes, run `go generate ./cmd/gophper`. It collects the licenses of the linked modules for `gophper licenses`, and a test fails without it.
- Library users get a sandbox: host paths, processes and the rest stay off in `Options` unless set. The CLI and `server` turn them on.

## Skills

Read the matching one in `.agents/skills/` before you start. Each lists the designs already rejected in its area, and why.

| Skill | When |
| --- | --- |
| `gophper-wasm-upgrade` | A new gophper-wasm release, a local checkout, or the host side of an ABI change |
| `dependency-upgrade` | Go modules, the Go toolchain, or tools in `mise.toml` |
| `wasm-host` | The Engine, instances, interrupts, signals, host access, `internal/dylink` |
| `server` | `serve` and `fcgi`: routing, workers, opcache, php.ini, options |
| `performance` | Measuring, or changing README's numbers |
| `declscope-authoring` | Adding, naming or moving any declaration |
| `declscope-adoption` | Changing declscope's configuration |

## Before you finish

```sh
go vet ./... && go test ./...
mise exec -- golangci-lint run ./...
mise exec -- declscope shrink ./...
mise exec -- declscope ./...
```

declscope runs with `.declscope.yaml` (`qualify: ondemand`, `exported: true`).

## Rejected designs

| Idea | Why not |
| --- | --- |
| Write the interpreter from scratch in Go | The standard library is far larger than the language core. |
