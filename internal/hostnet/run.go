// Package hostnet gives PHP sockets and DNS through host functions.
//
// gophper-wasm's compat/gophper_net.c is the guest side. It routes the POSIX socket
// calls to the functions ExportSockets and ExportDNS add, and each socket
// is a Go net.Conn, net.Listener or net.PacketConn.
package hostnet

import "context"

// Run is what the host functions need from the PHP instance calling them.
type Run interface {
	// Context is the run's context.
	Context() context.Context
	// Interruption is closed by the next interrupt, such as a timeout
	// firing. A blocking call cut short by it returns EINTR, as a signal
	// would make it.
	Interruption() <-chan struct{}
}

// interruptibleRunContext is canceled by the next interrupt or the end of the run.
//
//declscope:shared // socket.go dials and dns.go resolves with it
func interruptibleRunContext(run Run) (context.Context, context.CancelFunc) {
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
