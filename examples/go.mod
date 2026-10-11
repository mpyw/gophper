module example/gophper

go 1.27.1

require (
	github.com/mpyw/gophper v0.1.0
	github.com/tetratelabs/wazero v1.12.0
)

require (
	github.com/mpyw/gophper-wasm v0.2.3 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/mpyw/gophper => ../
