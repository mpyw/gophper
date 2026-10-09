// Package hostnet gives PHP sockets and DNS through host functions.
//
// gophper-wasm's compat/gophper_net.c is the guest side. It routes the POSIX socket
// calls to the functions ExportSockets and ExportDNS add, and each socket
// is a Go net.Conn, net.Listener or net.PacketConn.
package hostnet

import (
	"context"

	"github.com/mpyw/gophper/internal/wasi"
)

// interruptibleRunContext is canceled by the next interrupt or the end of the run.
//
//declscope:shared // socket.go dials and dns.go resolves with it
func interruptibleRunContext(run wasi.Run) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(run.Context())
	intr := run.Interruption()
	go func() {
		select {
		case <-intr:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
