---
name: dependency-upgrade
description: Update or add a Go module or tool in gophper, such as wazero, urfave/cli, golang.org/x/crypto, the Go toolchain, golangci-lint or declscope. Use when editing go.mod, mise.toml or .golangci.yml's allow lists. For the PHP binaries, use gophper-wasm-upgrade.
---

# Upgrade a dependency

## Steps

1. Update:

   ```sh
   go get -u ./... && go mod tidy
   go generate ./cmd/gophper   # licenses of the linked modules
   ```

   Tools are pinned in `mise.toml`. The `go` line in `go.mod` sets the toolchain.
2. A new module must be allowed in `.golangci.yml`. depguard keeps strict lists per package: the core, `server`, `internal/fcgi`, the command and the tests. Add it only where it is used.
3. Before adding a module, weigh what it links in. `go list -deps -f '{{with .Module}}{{.Path}}{{end}}' ./cmd/gophper | sort -u | wc -l` counts the modules, now 4.
4. Run AGENTS.md's "Before you finish", and `mise exec -- golangci-lint run ./...`.

## wazero

| Check | Why |
| --- | --- |
| `WithCoreFeatures` in `engine.go` | Exception handling and extended-const are experimental features. Their names can change. |
| `experimental.WithMemoryAllocator` | `engineMemory` allocates the linear memory itself (wasm-host skill) |
| wazero issue #2522 | `exnref` use-after-free, open on 2026-10-09. Check whether a release fixed it. |
| A few tests under `-race` | wazero compiles under the race detector, so run some at a time: `go test -race -timeout 40m -run 'TestX' .` |
| Performance | Run `testdata/bench.php` again (performance skill). The compiler shapes it. |

## Rejected designs

| Idea | Why not |
| --- | --- |
| Caddy built into `gophper` (a `caddy` subcommand and a Caddy module) | It took 160 of the command's 164 Go modules and 65 MB of its binary. Every option was written twice, as a flag and as a Caddyfile subdirective, and bugs such as the php.ini section one came twice. `serve` does automatic HTTPS for one or more domains. Anyone who needs more of Caddy puts it in front of `serve` or `fcgi`. |
