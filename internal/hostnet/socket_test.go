package hostnet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero/api"
)

// Where the harness puts things in guest memory.
const (
	socketAddrOff  = 0
	socketToOff    = 512
	socketOutOff   = 1024
	socketLenOff   = 2000
	socketValueOff = 2048
	socketBufOff   = 4096
	socketBufCap   = 32 << 10
	socketDataOff  = socketBufOff + socketBufCap
)

// The errnos the host's errors become.
var (
	socketEADDRINUSE   = errnoFrom(syscall.EADDRINUSE)
	socketECONNREFUSED = errnoFrom(syscall.ECONNREFUSED)
)

// socketHarness calls the socket host functions as the guest would.
type socketHarness struct {
	t   *testing.T
	m   api.Module
	run *guestRun
	tab *Sockets
	x   socketExports
	ctx context.Context
}

func newSocketHarness(t *testing.T) *socketHarness {
	t.Helper()
	run := newGuestRun(t)
	tab := NewSockets(run, func(path string) (string, bool, bool) { return path, true, true }, true)
	t.Cleanup(tab.Close)
	return &socketHarness{
		t:   t,
		m:   newGuest(t),
		run: run,
		tab: tab,
		x:   socketExports{from: func(context.Context) *Sockets { return tab }},
		ctx: context.Background(),
	}
}

func (h *socketHarness) open(fd, kind int32) {
	h.t.Helper()
	if errno := h.x.open(h.ctx, fd, kind); errno != 0 {
		h.t.Fatalf("open(%d, %d) = %d", fd, kind, errno)
	}
}

func (h *socketHarness) connect(fd int32, addr string, nonblock int32) int32 {
	p, n := guestString(h.t, h.m, socketAddrOff, addr)
	return h.x.connect(h.ctx, h.m, fd, p, n, nonblock)
}

func (h *socketHarness) bind(fd int32, addr string) int32 {
	p, n := guestString(h.t, h.m, socketAddrOff, addr)
	return h.x.bind(h.ctx, h.m, fd, p, n)
}

func (h *socketHarness) send(fd int32, data, to string, flags int32) int32 {
	bp, bn := guestString(h.t, h.m, socketDataOff, data)
	var tp, tn uint32
	if to != "" {
		tp, tn = guestString(h.t, h.m, socketToOff, to)
	}
	return h.x.send(h.ctx, h.m, fd, bp, int32(bn), flags, tp, tn)
}

// recv returns what it read, the sender, and the result.
func (h *socketHarness) recv(fd, n, flags int32) (string, string, int32) {
	h.m.Memory().WriteUint32Le(socketLenOff, 0)
	r := h.x.recv(h.ctx, h.m, fd, socketBufOff, n, flags, socketOutOff, 256, socketLenOff)
	if r <= 0 {
		return "", "", r
	}
	fromLen, _ := h.m.Memory().ReadUint32Le(socketLenOff)
	return guestRead(h.t, h.m, socketBufOff, uint32(r)), guestRead(h.t, h.m, socketOutOff, fromLen), r
}

// recvAll reads until n bytes have arrived.
func (h *socketHarness) recvAll(fd int32, n int) string {
	h.t.Helper()
	var b strings.Builder
	for b.Len() < n {
		data, _, r := h.recv(fd, socketBufCap, 0)
		if r <= 0 {
			h.t.Fatalf("recv = %d after %d bytes", r, b.Len())
		}
		_, _ = b.WriteString(data)
	}
	return b.String()
}

func (h *socketHarness) name(fd, peer int32, capacity uint32) (string, int32) {
	h.m.Memory().WriteUint32Le(socketLenOff, 0)
	errno := h.x.name(h.ctx, h.m, fd, peer, socketOutOff, capacity, socketLenOff)
	n, _ := h.m.Memory().ReadUint32Le(socketLenOff)
	return guestRead(h.t, h.m, socketOutOff, n), errno
}

func (h *socketHarness) getopt(fd, opt int32) (int32, int32) {
	errno := h.x.getopt(h.ctx, h.m, fd, opt, socketValueOff)
	v, _ := h.m.Memory().ReadUint32Le(socketValueOff)
	return int32(v), errno
}

func (h *socketHarness) entry(fd int32) *socketEntry {
	h.tab.mu.Lock()
	defer h.tab.mu.Unlock()
	return h.tab.entries[fd]
}

// await waits until ready holds for the socket, as a blocking call would.
func (h *socketHarness) await(fd int32, ready func(e *socketEntry) bool) {
	h.t.Helper()
	h.tab.mu.Lock()
	defer h.tab.mu.Unlock()
	e := h.tab.entries[fd]
	if errno := h.tab.wait(func() bool { return ready(e) }, 2*time.Second); errno != 0 {
		h.t.Fatalf("wait = %d", errno)
	}
}

// socketEchoListener answers on each connection with what it reads.
func socketEchoListener(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				// The copy ends when either side closes; the client checks what came back.
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l
}

// socketEchoPacket answers each datagram with "echo: " and the datagram.
func socketEchoPacket(t *testing.T) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			// A lost echo shows as a failed recv in the test.
			_, _ = pc.WriteTo(append([]byte("echo: "), buf[:n]...), from)
		}
	}()
	return pc
}

// socketClosedPort returns a local TCP address nothing listens on.
func socketClosedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestSocketUnknownFD(t *testing.T) {
	h := newSocketHarness(t)
	const fd = 99
	name, nameErr := h.name(fd, 0, 64)
	_, optErr := h.getopt(fd, socketOptRcvbuf)
	_, _, recvErr := h.recv(fd, 10, 0)
	got := map[string]int32{
		"open":      h.x.open(h.ctx, fd, 9),
		"connect":   h.connect(fd, "127.0.0.1:1", 0),
		"bind":      h.bind(fd, "127.0.0.1:0"),
		"listen":    h.x.listen(h.ctx, fd, 1),
		"accept":    h.x.accept(h.ctx, fd, 100, 1),
		"recv":      recvErr,
		"send":      h.send(fd, "x", "", 0),
		"shutdown":  h.x.shutdown(h.ctx, fd, 3),
		"name":      nameErr,
		"getopt":    optErr,
		"setopt":    h.x.setopt(h.ctx, fd, socketOptNodelay, 1),
		"available": h.x.available(h.ctx, fd),
		"close":     h.x.close(h.ctx, fd),
	}
	want := map[string]int32{
		"open":      wasi.EPROTONOSUPPORT,
		"connect":   wasi.EBADF,
		"bind":      wasi.EBADF,
		"listen":    wasi.EBADF,
		"accept":    wasi.EINVAL,
		"recv":      -wasi.EBADF,
		"send":      -wasi.EBADF,
		"shutdown":  wasi.EBADF,
		"name":      wasi.EBADF,
		"getopt":    wasi.EBADF,
		"setopt":    wasi.EBADF,
		"available": -wasi.EBADF,
		"close":     0,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
	if name != "" {
		t.Errorf("name wrote %q", name)
	}
}

func TestSocketOptions(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if v, errno := h.getopt(1, socketOptRcvbuf); errno != 0 || v != 65536 {
		t.Errorf("default SO_RCVBUF = %d, %d", v, errno)
	}
	for _, opt := range []struct {
		opt, value int32
	}{
		{socketOptNodelay, 1}, {socketOptKeepalive, 1}, {socketOptReuseaddr, 1},
		{socketOptBroadcast, 1}, {socketOptRcvbuf, 4096}, {socketOptSndbuf, 8192},
	} {
		if errno := h.x.setopt(h.ctx, 1, opt.opt, opt.value); errno != 0 {
			t.Errorf("setopt(%d) = %d", opt.opt, errno)
		}
		if v, errno := h.getopt(1, opt.opt); errno != 0 || v != opt.value {
			t.Errorf("getopt(%d) = %d, %d, want %d", opt.opt, v, errno, opt.value)
		}
		if errno := h.x.setopt(h.ctx, 1, opt.opt, 0); errno != 0 {
			t.Errorf("setopt(%d, 0) = %d", opt.opt, errno)
		}
		if v, _ := h.getopt(1, opt.opt); v != 0 {
			t.Errorf("getopt(%d) after clearing = %d", opt.opt, v)
		}
	}
	if errno := h.x.setopt(h.ctx, 1, 99, 1); errno != wasi.EINVAL {
		t.Errorf("setopt(unknown) = %d", errno)
	}
	if _, errno := h.getopt(1, 99); errno != wasi.EINVAL {
		t.Errorf("getopt(unknown) = %d", errno)
	}
	if v, errno := h.getopt(1, socketOptError); errno != 0 || v != 0 {
		t.Errorf("SO_ERROR = %d, %d", v, errno)
	}

	// On a connected socket, the options reach the host's.
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	if errno := h.connect(1, l.Addr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	for _, opt := range []int32{socketOptNodelay, socketOptKeepalive, socketOptRcvbuf, socketOptSndbuf} {
		if errno := h.x.setopt(h.ctx, 1, opt, 16384); errno != 0 {
			t.Errorf("setopt(%d) when connected = %d", opt, errno)
		}
	}
	if v, _ := h.getopt(1, socketOptNodelay); v != 1 {
		t.Errorf("TCP_NODELAY = %d", v)
	}
}

// An accepted socket inherits the listener's options.
func TestSocketAcceptInheritsOptions(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	h.x.setopt(h.ctx, 1, socketOptNodelay, 1)
	h.x.setopt(h.ctx, 1, socketOptKeepalive, 1)
	h.x.setopt(h.ctx, 1, socketOptRcvbuf, 1234)
	if errno := h.x.listen(h.ctx, 1, 8); errno != 0 {
		t.Fatalf("listen = %d", errno)
	}
	if errno := h.x.listen(h.ctx, 1, 8); errno != 0 {
		t.Errorf("listen again = %d", errno)
	}
	if errno := h.x.accept(h.ctx, 1, 2, 1); errno != wasi.EAGAIN {
		t.Errorf("non-blocking accept with nobody waiting = %d", errno)
	}
	addr, _ := h.name(1, 0, 64)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if errno := h.x.accept(h.ctx, 1, 2, 0); errno != 0 {
		t.Fatalf("accept = %d", errno)
	}
	for opt, want := range map[int32]int32{socketOptNodelay: 1, socketOptKeepalive: 1, socketOptRcvbuf: 1234} {
		if v, _ := h.getopt(2, opt); v != want {
			t.Errorf("accepted getopt(%d) = %d, want %d", opt, v, want)
		}
	}
	if peer, _ := h.name(2, 1, 64); peer != c.LocalAddr().String() {
		t.Errorf("peer = %q, want %q", peer, c.LocalAddr())
	}
	if _, err := c.Write([]byte("from client")); err != nil {
		t.Fatal(err)
	}
	if got := h.recvAll(2, len("from client")); got != "from client" {
		t.Errorf("recv = %q", got)
	}
	if errno := h.x.accept(h.ctx, 2, 3, 1); errno != wasi.EINVAL {
		t.Errorf("accept on a connection = %d", errno)
	}
}

func TestSocketAcceptAfterListenerFails(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	h.x.listen(h.ctx, 1, 8)
	// The listener dies under the socket, as on a host error.
	if err := h.entry(1).listener.Close(); err != nil {
		t.Fatal(err)
	}
	if errno := h.x.accept(h.ctx, 1, 2, 0); errno != wasi.EBADF {
		t.Errorf("accept = %d, want EBADF", errno)
	}
}

func TestSocketConnectStates(t *testing.T) {
	h := newSocketHarness(t)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")

	h.open(1, socketTCP)
	h.x.listen(h.ctx, 1, 1)
	if errno := h.connect(1, l.Addr().String(), 0); errno != wasi.EINVAL {
		t.Errorf("connect on a listener = %d", errno)
	}

	h.open(2, socketTCP)
	if errno := h.bind(2, "127.0.0.1:0"); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	if local, _ := h.name(2, 0, 64); local != "127.0.0.1:0" {
		t.Errorf("name before connect = %q", local)
	}
	if errno := h.connect(2, l.Addr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if local, _ := h.name(2, 0, 64); !strings.HasPrefix(local, "127.0.0.1:") || local == "127.0.0.1:0" {
		t.Errorf("local name = %q", local)
	}
	if errno := h.connect(2, l.Addr().String(), 0); errno != wasi.EISCONN {
		t.Errorf("connect twice = %d", errno)
	}
	if errno := h.bind(2, "127.0.0.1:0"); errno != wasi.EINVAL {
		t.Errorf("bind after connect = %d", errno)
	}
	if n := h.send(2, "ping", "", 0); n != 4 {
		t.Errorf("send = %d", n)
	}
	if got := h.recvAll(2, 4); got != "ping" {
		t.Errorf("echo = %q", got)
	}

	// While a non-blocking connect is under way.
	h.open(3, socketTCP)
	h.tab.mu.Lock()
	h.tab.entries[3].connecting = true
	h.tab.mu.Unlock()
	if errno := h.connect(3, l.Addr().String(), 1); errno != wasi.EALREADY {
		t.Errorf("connect while connecting = %d", errno)
	}
	if n := h.send(3, "x", "", socketMsgDontwait); n != -wasi.EAGAIN {
		t.Errorf("non-blocking send while connecting = %d", n)
	}
}

func TestSocketNonblockingConnect(t *testing.T) {
	h := newSocketHarness(t)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	h.open(1, socketTCP)
	if errno := h.connect(1, l.Addr().String(), 1); errno != wasi.EINPROGRESS {
		t.Fatalf("connect = %d, want EINPROGRESS", errno)
	}
	// A send waits for the connect to finish.
	if n := h.send(1, "early", "", 0); n != 5 {
		t.Errorf("send = %d", n)
	}
	if got := h.recvAll(1, 5); got != "early" {
		t.Errorf("echo = %q", got)
	}

	h.open(2, socketTCP)
	if errno := h.connect(2, socketClosedPort(t), 1); errno != wasi.EINPROGRESS {
		t.Fatalf("connect = %d, want EINPROGRESS", errno)
	}
	h.await(2, func(e *socketEntry) bool { return !e.connecting })
	if got := h.x.available(h.ctx, 2); got != 0 {
		t.Errorf("available = %d", got)
	}
	if _, _, r := h.recv(2, 10, 0); r != -socketECONNREFUSED {
		t.Errorf("recv = %d, want -ECONNREFUSED", r)
	}
	if n := h.send(2, "x", "", 0); n != -socketECONNREFUSED {
		t.Errorf("send = %d, want -ECONNREFUSED", n)
	}
	if v, _ := h.getopt(2, socketOptError); v != socketECONNREFUSED {
		t.Errorf("SO_ERROR = %d, want ECONNREFUSED", v)
	}
	if v, _ := h.getopt(2, socketOptError); v != 0 {
		t.Errorf("SO_ERROR read twice = %d", v)
	}
	if _, _, r := h.recv(2, 10, 0); r != -wasi.ENOTCONN {
		t.Errorf("recv after SO_ERROR = %d, want -ENOTCONN", r)
	}
}

func TestSocketBlockingConnectRefused(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if errno := h.connect(1, socketClosedPort(t), 0); errno != socketECONNREFUSED {
		t.Errorf("connect = %d, want ECONNREFUSED", errno)
	}
	if n := h.send(1, "x", "", 0); n != -wasi.ENOTCONN {
		t.Errorf("send = %d, want -ENOTCONN", n)
	}
}

func TestSocketConnectAfterRunEnds(t *testing.T) {
	h := newSocketHarness(t)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	h.run.cancel()
	h.open(1, socketTCP)
	if errno := h.connect(1, l.Addr().String(), 0); errno != wasi.EINTR {
		t.Errorf("connect = %d, want EINTR", errno)
	}
}

func TestSocketBlockingCallsWake(t *testing.T) {
	h := newSocketHarness(t)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	h.open(1, socketTCP)
	h.connect(1, l.Addr().String(), 0)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(h.run.intr)
	}()
	if _, _, r := h.recv(1, 10, 0); r != -wasi.EINTR {
		t.Errorf("recv on interrupt = %d, want -EINTR", r)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.run.cancel()
	}()
	h.run.intr = make(chan struct{})
	if _, _, r := h.recv(1, 10, 0); r != -wasi.EIO {
		t.Errorf("recv once the run is over = %d, want -EIO", r)
	}
}

func TestSocketBindAndListen(t *testing.T) {
	h := newSocketHarness(t)

	h.open(1, socketUDP)
	if errno := h.bind(1, "127.0.0.1:0"); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	addr, _ := h.name(1, 0, 64)
	if errno := h.bind(1, "127.0.0.1:0"); errno != wasi.EINVAL {
		t.Errorf("bind twice = %d", errno)
	}
	if errno := h.x.listen(h.ctx, 1, 1); errno != wasi.ENOTSUP {
		t.Errorf("listen on UDP = %d", errno)
	}
	h.open(2, socketUDP)
	if errno := h.bind(2, addr); errno != socketEADDRINUSE {
		t.Errorf("bind to %s in use = %d, want EADDRINUSE", addr, errno)
	}

	h.open(3, socketUnix)
	if errno := h.x.listen(h.ctx, 3, 1); errno != wasi.EINVAL {
		t.Errorf("listen on an unbound Unix socket = %d", errno)
	}
	if name, errno := h.name(3, 0, 64); errno != 0 || name != "" {
		t.Errorf("name of an unbound Unix socket = %q, %d", name, errno)
	}

	// A TCP socket listens on its bound address, and fails as the host does.
	h.open(4, socketTCP)
	h.x.listen(h.ctx, 4, 1)
	taken, _ := h.name(4, 0, 64)
	h.open(5, socketTCP)
	h.bind(5, taken)
	if errno := h.x.listen(h.ctx, 5, 1); errno != socketEADDRINUSE {
		t.Errorf("listen on %s in use = %d, want EADDRINUSE", taken, errno)
	}
}

func TestSocketName(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if name, _ := h.name(1, 0, 64); name != "0.0.0.0:0" {
		t.Errorf("unbound TCP name = %q", name)
	}
	if _, errno := h.name(1, 1, 64); errno != wasi.ENOTCONN {
		t.Errorf("peer of an unconnected socket = %d", errno)
	}
	h.open(2, socketUDP)
	if name, _ := h.name(2, 0, 64); name != "0.0.0.0:0" {
		t.Errorf("unbound UDP name = %q", name)
	}
	// The name is cut to fit.
	h.bind(1, "127.0.0.1:8080")
	if name, _ := h.name(1, 0, 5); name != "127.0" {
		t.Errorf("cut name = %q", name)
	}
	// Without a place for the length, nothing is written.
	h.m.Memory().WriteString(socketOutOff, "untouched")
	if errno := h.x.name(h.ctx, h.m, 1, 0, socketOutOff, 64, 0); errno != 0 || guestRead(t, h.m, socketOutOff, 9) != "untouched" {
		t.Errorf("name without a length = %d", errno)
	}
}

func TestSocketUDP(t *testing.T) {
	h := newSocketHarness(t)
	server := socketEchoPacket(t)

	// Connected: send and recv without addresses.
	h.open(1, socketUDP)
	if errno := h.connect(1, server.LocalAddr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if peer, _ := h.name(1, 1, 64); peer != server.LocalAddr().String() {
		t.Errorf("peer = %q", peer)
	}
	if n := h.send(1, "one", "", 0); n != 3 {
		t.Errorf("send = %d", n)
	}
	h.await(1, func(e *socketEntry) bool { return len(e.packets) > 0 })
	if n := h.x.available(h.ctx, 1); n != int32(len("echo: one")) {
		t.Errorf("available = %d", n)
	}
	if data, from, _ := h.recv(1, 100, socketMsgPeek); data != "echo: one" || from != server.LocalAddr().String() {
		t.Errorf("peek = %q from %q", data, from)
	}
	if data, _, _ := h.recv(1, 4, 0); data != "echo" {
		t.Errorf("short recv = %q", data)
	}
	if n := h.x.available(h.ctx, 1); n != 0 {
		t.Errorf("available after recv = %d", n)
	}
	if _, _, r := h.recv(1, 100, socketMsgDontwait); r != -wasi.EAGAIN {
		t.Errorf("non-blocking recv with nothing queued = %d", r)
	}
	// sendto on a connected socket: to its peer, from its own port.
	if n := h.send(1, "again", server.LocalAddr().String(), 0); n != 5 {
		t.Errorf("sendto the peer = %d", n)
	}
	if data, _, _ := h.recv(1, 100, 0); data != "echo: again" {
		t.Errorf("recv = %q", data)
	}
	if n := h.send(1, "x", "127.0.0.1:9", 0); n != -wasi.EISCONN {
		t.Errorf("sendto another address = %d, want -EISCONN", n)
	}
	if e := h.entry(1); e.packet != nil {
		t.Error("sendto bound a second socket")
	}

	// Unbound: sendto binds it.
	h.open(2, socketUDP)
	if _, _, r := h.recv(2, 100, 0); r != -wasi.ENOTCONN {
		t.Errorf("recv on an unbound socket = %d", r)
	}
	if n := h.send(2, "x", "", 0); n != -wasi.EDESTADDRREQ {
		t.Errorf("send without a peer = %d", n)
	}
	if n := h.send(2, "two", server.LocalAddr().String(), 0); n != 3 {
		t.Fatalf("sendto = %d", n)
	}
	if local, _ := h.name(2, 0, 64); strings.HasSuffix(local, ":0") {
		t.Errorf("name after sendto = %q", local)
	}
	if data, from, _ := h.recv(2, 100, 0); data != "echo: two" || from != server.LocalAddr().String() {
		t.Errorf("recv = %q from %q", data, from)
	}
	if n := h.send(2, "x", "no port", 0); n >= 0 {
		t.Errorf("sendto a bad address = %d", n)
	}

	h.open(3, socketUDP)
	if errno := h.connect(3, "no port", 0); errno == 0 {
		t.Error("connect to a bad address succeeded")
	}
	if errno := h.bind(3, "no port"); errno == 0 {
		t.Error("bind to a bad address succeeded")
	}

	// recv returns once the socket is closed under it.
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.tab.mu.Lock()
		h.tab.entries[2].close()
		h.tab.notify()
		h.tab.mu.Unlock()
	}()
	if _, _, r := h.recv(2, 100, 0); r != -wasi.EBADF {
		t.Errorf("recv on a closed socket = %d", r)
	}
}

func TestSocketUnixgram(t *testing.T) {
	// macOS limits socket paths to 104 bytes, so not under t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := dir + "/s.sock"

	h := newSocketHarness(t)
	h.open(1, socketUnixgram)
	if errno := h.bind(1, path); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	h.open(2, socketUnixgram)
	if errno := h.connect(2, path, 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if n := h.send(2, "dgram", "", 0); n != 5 {
		t.Errorf("send = %d", n)
	}
	if data, _, _ := h.recv(1, 100, 0); data != "dgram" {
		t.Errorf("recv = %q", data)
	}
	if n := h.send(1, "back", path, 0); n != 4 {
		t.Errorf("sendto from a bound socket = %d", n)
	}
	if data, _, _ := h.recv(1, 100, 0); data != "back" {
		t.Errorf("recv = %q", data)
	}
	h.open(3, socketUnixgram)
	if n := h.send(3, "x", path, 0); n != -wasi.ENOTSUP {
		t.Errorf("sendto from an unbound Unix socket = %d, want -ENOTSUP", n)
	}
	if n := h.send(1, "x", dir+"/none.sock", 0); n >= 0 {
		t.Errorf("sendto nobody = %d", n)
	}
}

func TestSocketSendErrors(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if n := h.send(1, "x", "", 0); n != -wasi.ENOTCONN {
		t.Errorf("send unconnected = %d", n)
	}
	if n := h.x.send(h.ctx, h.m, 1, 1<<16-1, 100, 0, 0, 0); n != -wasi.EINVAL {
		t.Errorf("send from outside memory = %d", n)
	}
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	h.connect(1, l.Addr().String(), 0)
	if err := h.entry(1).conn.Close(); err != nil {
		t.Fatal(err)
	}
	if n := h.send(1, "x", "", 0); n != -wasi.EPIPE {
		t.Errorf("send on a closed connection = %d, want -EPIPE", n)
	}
}

func TestSocketShutdown(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if errno := h.x.shutdown(h.ctx, 1, 3); errno != wasi.ENOTCONN {
		t.Errorf("shutdown unconnected = %d", errno)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	h.connect(1, l.Addr().String(), 0)
	peer, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()

	if errno := h.x.shutdown(h.ctx, 1, 2); errno != 0 {
		t.Fatalf("shutdown(SHUT_WR) = %d", errno)
	}
	if b, err := io.ReadAll(peer); err != nil || len(b) != 0 {
		t.Errorf("peer read %q, %v", b, err)
	}
	if n := h.send(1, "x", "", 0); n != -wasi.EPIPE {
		t.Errorf("send after SHUT_WR = %d, want -EPIPE", n)
	}
	if ev := h.entry(1).events(socketPollOut); ev&socketPollOut != 0 {
		t.Errorf("writable after SHUT_WR: %d", ev)
	}
	// Reading still works, until the peer closes.
	if _, err := peer.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := h.recvAll(1, 3); got != "bye" {
		t.Errorf("recv = %q", got)
	}
	if _, _, r := h.recv(1, 10, 0); r != 0 {
		t.Errorf("recv at EOF = %d", r)
	}
	// Both directions are closed now. Linux's shutdown(2) still succeeds,
	// and macOS's reports ENOTCONN.
	if errno := h.x.shutdown(h.ctx, 1, 1); errno != 0 && errno != wasi.ENOTCONN {
		t.Errorf("shutdown(SHUT_RD) = %d", errno)
	}

	// A pipe end has no halves to close.
	h.x.pipeOpen(h.ctx, 10, 11)
	if errno := h.x.shutdown(h.ctx, 11, 3); errno != 0 {
		t.Errorf("shutdown on a pipe = %d", errno)
	}
}

func TestSocketPipe(t *testing.T) {
	h := newSocketHarness(t)
	if errno := h.x.pipeOpen(h.ctx, 1, 2); errno != 0 {
		t.Fatalf("pipe_open = %d", errno)
	}
	if ev := h.entry(2).events(socketPollIn | socketPollOut); ev != socketPollOut {
		t.Errorf("write end events = %d", ev)
	}
	if _, _, r := h.recv(2, 10, 0); r != -wasi.EBADF {
		t.Errorf("recv on the write end = %d", r)
	}
	if name, errno := h.name(1, 1, 64); errno != 0 || name != "" {
		t.Errorf("pipe peer name = %q, %d", name, errno)
	}
	if a := h.entry(1).conn.LocalAddr(); a.Network() != "pipe" || a.String() != "" {
		t.Errorf("pipe address = %s %q", a.Network(), a.String())
	}

	// Before anything reads it, a child may take an end.
	f, done, err := h.tab.ChildFile(1)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if f == nil {
		t.Fatal("no file")
	}
	if n := h.send(2, "through", "", 0); n != 7 {
		t.Errorf("send = %d", n)
	}
	if got := h.recvAll(1, 7); got != "through" {
		t.Errorf("recv = %q", got)
	}
	if _, _, err := h.tab.ChildFile(1); err == nil {
		t.Error("ChildFile after reading ahead succeeded")
	}
	if n := h.x.available(h.ctx, 1); n != 0 {
		t.Errorf("available = %d", n)
	}
	if _, _, err := h.tab.ChildFile(42); !errors.Is(err, os.ErrClosed) {
		t.Errorf("ChildFile of an unknown fd: %v", err)
	}
	h.open(3, socketTCP)
	if _, _, err := h.tab.ChildFile(3); err == nil {
		t.Error("ChildFile of an unconnected socket succeeded")
	}
}

func TestSocketPair(t *testing.T) {
	h := newSocketHarness(t)
	if errno := h.x.pair(h.ctx, 1, 2); errno != 0 {
		t.Fatalf("sock_pair = %d", errno)
	}
	// A child takes a duplicate of a stream socket.
	f, done, err := h.tab.ChildFile(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("from child")); err != nil {
		t.Fatal(err)
	}
	done()
	if got := h.recvAll(1, 10); got != "from child" {
		t.Errorf("recv = %q", got)
	}

	// More than the read-ahead limit waits for PHP to read.
	big := bytes.Repeat([]byte("0123456789abcdef"), (socketMaxBuffered+socketBufCap)/16*2)
	go func() {
		c := h.entry(2).conn
		if _, err := c.Write(big); err != nil {
			t.Error(err)
		}
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	h.x.available(h.ctx, 1)
	h.await(1, func(e *socketEntry) bool { return len(e.recvBuf) >= socketMaxBuffered })
	if n := int(h.x.available(h.ctx, 1)); n < socketMaxBuffered || n > socketMaxBuffered+64<<10 {
		t.Errorf("buffered %d bytes", n)
	}
	if got := h.recvAll(1, len(big)); got != string(big) {
		t.Errorf("recv %d bytes, want %d", len(got), len(big))
	}
}

func TestSocketCloseAll(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	h.x.listen(h.ctx, 1, 1)
	addr, _ := h.name(1, 0, 64)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h.await(1, func(e *socketEntry) bool { return len(e.pending) > 0 })
	h.open(2, socketUDP)
	h.bind(2, "127.0.0.1:0")
	h.tab.Close()
	if len(h.tab.entries) != 0 {
		t.Errorf("%d sockets left", len(h.tab.entries))
	}
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("a pending connection stayed open")
	}
}

func TestSocketPlaceholderFS(t *testing.T) {
	b, err := fs.ReadFile(SocketPlaceholderFS, "socket")
	if err != nil || len(b) != 0 {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
	fi, err := fs.Stat(SocketPlaceholderFS, "socket")
	if err != nil || fi.Name() != "socket" || fi.IsDir() || fi.Mode() != 0o444 || fi.Size() != 0 || !fi.ModTime().IsZero() || fi.Sys() != nil {
		t.Errorf("Stat(socket) = %v, %v", fi, err)
	}
	di, err := fs.Stat(SocketPlaceholderFS, ".")
	if err != nil || di.Name() != "." || !di.IsDir() || !di.Mode().IsDir() {
		t.Errorf("Stat(.) = %v, %v", di, err)
	}
	if entries, err := fs.ReadDir(SocketPlaceholderFS, "."); err != nil || len(entries) != 0 {
		t.Errorf("ReadDir = %v, %v", entries, err)
	}
	if _, err := SocketPlaceholderFS.Open("other"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(other) = %v", err)
	}
}

// When the host refuses an option, setsockopt fails and the option keeps its
// old value.
func TestSocketSetoptHostFailure(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	if errno := h.connect(1, l.Addr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	before := map[int32]int32{}
	for _, opt := range []int32{socketOptNodelay, socketOptKeepalive, socketOptRcvbuf, socketOptSndbuf} {
		before[opt], _ = h.getopt(1, opt)
	}
	if err := h.entry(1).conn.Close(); err != nil {
		t.Fatal(err)
	}
	for opt, old := range before {
		value := int32(1)
		if opt == socketOptRcvbuf || opt == socketOptSndbuf {
			value = 4096
		}
		if old == value {
			value = 0
		}
		if errno := h.x.setopt(h.ctx, 1, opt, value); errno != wasi.EBADF {
			t.Errorf("setopt(%d) on a closed connection = %d, want EBADF", opt, errno)
		}
		if v, _ := h.getopt(1, opt); v != old {
			t.Errorf("getopt(%d) after a failed setopt = %d, want %d", opt, v, old)
		}
	}
}

// When the host's shutdown fails, the guest gets the errno.
func TestSocketShutdownHostFailure(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	if errno := h.connect(1, l.Addr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if err := h.entry(1).conn.Close(); err != nil {
		t.Fatal(err)
	}
	for _, how := range []int32{1, 2, 3} {
		if errno := h.x.shutdown(h.ctx, 1, how); errno != wasi.EBADF {
			t.Errorf("shutdown(%d) on a closed connection = %d, want EBADF", how, errno)
		}
	}
	if h.entry(1).shutWrite {
		t.Error("shutWrite set after a failed shutdown")
	}
}
