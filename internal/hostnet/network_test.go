//declscope:namespace socket

package hostnet

import (
	"testing"

	"github.com/mpyw/gophper/internal/wasi"
)

// TestSocketsNoNetwork refuses TCP and UDP, as a firewall would, with no
// host socket made. Unix sockets still go through hostPath.
func TestSocketsNoNetwork(t *testing.T) {
	h := newSocketHarness(t)
	h.tab.network = false
	for _, tt := range []struct {
		name string
		kind int32
		do   func(fd int32) int32
		want int32
	}{
		{"connect TCP", socketTCP, func(fd int32) int32 { return h.connect(fd, "127.0.0.1:1", 0) }, wasi.EACCES},
		{"connect UDP", socketUDP, func(fd int32) int32 { return h.connect(fd, "127.0.0.1:1", 0) }, wasi.EACCES},
		{"bind TCP", socketTCP, func(fd int32) int32 { return h.bind(fd, "127.0.0.1:0") }, wasi.EACCES},
		{"bind UDP", socketUDP, func(fd int32) int32 { return h.bind(fd, "127.0.0.1:0") }, wasi.EACCES},
		{"listen without bind", socketTCP, func(fd int32) int32 { return h.x.listen(h.ctx, fd, 1) }, wasi.EACCES},
		{"sendto unbound", socketUDP, func(fd int32) int32 { return -h.send(fd, "x", "127.0.0.1:1", 0) }, wasi.EACCES},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h.open(10, tt.kind)
			defer h.x.close(h.ctx, 10)
			if got := tt.do(10); got != tt.want {
				t.Errorf("errno %d, want %d", got, tt.want)
			}
			if e := h.entry(10); e.conn != nil || e.packet != nil || e.listener != nil {
				t.Error("a host socket was made")
			}
		})
	}
	if h.tab.Network() {
		t.Error("Network() says yes")
	}
}
