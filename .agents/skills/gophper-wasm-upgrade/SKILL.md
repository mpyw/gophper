---
name: gophper-wasm-upgrade
description: Move gophper to a new gophper-wasm release, test gophper against a local gophper-wasm checkout, or implement the host side of an ABI change. Use when go.mod's gophper-wasm version changes, when engineABIVersion or a "gophper" host function changes, or before a gophper-wasm release.
---

# Upgrade gophper-wasm

gophper-wasm's branches hold no binaries. Only its release tags carry them.

## Before a release: test against a local checkout

1. Build the checkout: `scripts/fetch.sh && scripts/deps.sh && scripts/build.sh && scripts/build-ext.sh` in `../gophper-wasm`.
2. Point Go at it with a `go.work` outside the repository, so that it is never committed:

   ```sh
   printf 'go 1.27.1\n\nuse (\n\t%s\n\t%s\n)\n' "$PWD" "$PWD/../gophper-wasm" > /tmp/gophper.work
   GOWORK=/tmp/gophper.work go test ./...
   ```

   The `go` line must not be older than either `go.mod`.

## After a release

```sh
GOPRIVATE='github.com/mpyw/*' go get github.com/mpyw/gophper-wasm@vX.Y.Z
go mod tidy
go generate ./cmd/gophper   # the license list names the version
```

Then run everything in AGENTS.md's "Before you finish".

| If the release changed | Also |
| --- | --- |
| `ABIVersion` | Set `engineABIVersion` in `engine.go` to it, and implement the host side (below) |
| The extensions | README's extension table, and `extension_test.go` |

## The host side of an ABI change

gophper-wasm's ABI.md is the contract. Every host function is in the host module "gophper", built in `engine.go`:

| Area | Exported by |
| --- | --- |
| Timer | `engine.go` (`set_timeout`) |
| Signals | `signal.go` |
| Sockets, pipes, DNS | `internal/hostnet` (`ExportSockets`, `ExportDNS`) |
| Processes | `internal/hostproc` (`ExportProcesses`) |
| Users, locks, paths | `internal/hostsys` (`ExportSystem`) |
| Go functions | `internal/hostfn` (`ExportFunctions`) |

A blocking call returns `EINTR` on an interrupt and `EIO` once the run is over, as ABI.md says. Read the wasm-host skill first.

## Rejected designs

| Idea | Why not |
| --- | --- |
| Ignoring the wasm binaries in Git | `go build` would need wasi-sdk, bison and re2c. They are committed in gophper-wasm instead. |
