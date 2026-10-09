//declscope:namespace socket

package hostnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero"
)

// errSocketBroken is a host error that maps to no errno of its own.
var errSocketBroken = errors.New("broken")

// socketBrokenConn is a connection whose every read and write fails, as a
// socket the host broke would. Nothing else of net.Conn is called on it.
type socketBrokenConn struct{ net.Conn }

func (socketBrokenConn) Read([]byte) (int, error)  { return 0, errSocketBroken }
func (socketBrokenConn) Write([]byte) (int, error) { return 0, errSocketBroken }
func (socketBrokenConn) Close() error              { return nil }
func (socketBrokenConn) RemoteAddr() net.Addr      { return socketPipeAddr{} }

// socketOneConnListener accepts conn, for driving acceptLoop by hand.
type socketOneConnListener struct{ conn net.Conn }

func (l socketOneConnListener) Accept() (net.Conn, error) { return l.conn, nil }
func (socketOneConnListener) Close() error                { return nil }
func (socketOneConnListener) Addr() net.Addr              { return socketPipeAddr{} }

// socketShortTempDir returns a directory for a Unix socket. macOS limits
// socket paths to 104 bytes, so not under t.TempDir(). Windows has no /tmp,
// and its temporary directory is short enough.
func socketShortTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/tmp", "gophper")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// socketPoll calls sock_poll and returns the revents and the result.
func socketPoll(h *socketHarness, fds, events []int32, timeoutMs int32) ([]int32, int32) {
	h.t.Helper()
	const fdsOff, eventsOff, reventsOff = socketBufOff, socketBufOff + 256, socketBufOff + 512
	for i := range fds {
		h.m.Memory().WriteUint32Le(fdsOff+uint32(i)*4, uint32(fds[i]))
		h.m.Memory().WriteUint32Le(eventsOff+uint32(i)*4, uint32(events[i]))
		h.m.Memory().WriteUint32Le(reventsOff+uint32(i)*4, 0)
	}
	r := h.x.poll(h.ctx, h.m, fdsOff, eventsOff, reventsOff, int32(len(fds)), timeoutMs)
	got := make([]int32, len(fds))
	for i := range got {
		v, _ := h.m.Memory().ReadUint32Le(reventsOff + uint32(i)*4)
		got[i] = int32(v)
	}
	return got, r
}

// socketPeerClosed reports whether the peer of c, one end of a net.Pipe,
// was closed. Such an end refuses a deadline once its peer is gone, and
// otherwise reads io.EOF.
func socketPeerClosed(t *testing.T, c net.Conn) bool {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return errors.Is(err, io.ErrClosedPipe)
	}
	_, err := c.Read(make([]byte, 1))
	return errors.Is(err, io.EOF)
}

// Every host function is exported under the name gophper-wasm imports.
func TestSocketExportSockets(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer func() { _ = r.Close(ctx) }()
	b := r.NewHostModuleBuilder("gophper")
	ExportSockets(b, func(context.Context) *Sockets { return nil })
	m, err := b.Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defs := m.ExportedFunctionDefinitions()
	for _, name := range []string{"sock_open", "sock_poll", "pipe_open", "sock_pair"} {
		if defs[name] == nil {
			t.Errorf("%s is not exported", name)
		}
	}
	if len(defs) != 16 {
		t.Errorf("%d functions exported, want 16", len(defs))
	}
}

// A Unix socket's path goes through hostPath, which may refuse it.
func TestSocketUnixHostPathRefused(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketUnix)
	h.open(2, socketUnixgram)

	h.tab.hostPath = nil
	if errno := h.connect(1, "/s.sock", 0); errno != wasi.ENOENT {
		t.Errorf("connect without host paths = %d, want ENOENT", errno)
	}

	h.tab.hostPath = func(string) (string, bool, bool) { return "", false, false }
	if errno := h.bind(1, "/s.sock"); errno != wasi.ENOENT {
		t.Errorf("bind to an unmapped path = %d, want ENOENT", errno)
	}

	h.tab.hostPath = func(path string) (string, bool, bool) { return path, false, true }
	if errno := h.bind(2, "/s.sock"); errno != wasi.EROFS {
		t.Errorf("bind to a read-only path = %d, want EROFS", errno)
	}
}

// A change can race the timer without a notify. wait then looks once more.
func TestSocketWaitReadyAtTimeout(t *testing.T) {
	h := newSocketHarness(t)
	looks := 0
	h.tab.mu.Lock()
	errno := h.tab.wait(func() bool { looks++; return looks > 1 }, 0)
	h.tab.mu.Unlock()
	if errno != 0 {
		t.Errorf("wait = %d, want 0", errno)
	}
}

// A datagram socket keeps the newest 256 datagrams, as a full buffer drops.
func TestSocketPacketQueueDropsOldest(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketUDP)
	if errno := h.bind(1, "127.0.0.1:0"); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	addr, _ := h.name(1, 0, 64)
	to, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const total = 300
	for i := range total {
		want := fmt.Sprint(i)
		if _, err := c.WriteTo([]byte(want), to); err != nil {
			t.Fatal(err)
		}
		// One at a time, so that no datagram is lost on the way.
		h.await(1, func(e *socketEntry) bool {
			return len(e.packets) > 0 && string(e.packets[len(e.packets)-1].data) == want
		})
	}
	e := h.entry(1)
	h.tab.mu.Lock()
	n, first := len(e.packets), string(e.packets[0].data)
	h.tab.mu.Unlock()
	if n != 256 || first != fmt.Sprint(total-256) {
		t.Errorf("%d datagrams queued, oldest %q", n, first)
	}
}

// A connection accepted after the socket closed is closed, as nobody takes it.
func TestSocketAcceptLoopAfterClose(t *testing.T) {
	h := newSocketHarness(t)
	ours, theirs := net.Pipe()
	defer func() { _ = theirs.Close() }()
	e := &socketEntry{kind: socketTCP, closed: true}
	h.tab.acceptLoop(e, socketOneConnListener{conn: ours})
	if len(e.pending) != 0 {
		t.Errorf("%d connections pending", len(e.pending))
	}
	if !socketPeerClosed(t, theirs) {
		t.Error("the connection stayed open")
	}
}

// A connect that finishes after the guest closed the socket is closed.
func TestSocketAttachAfterClose(t *testing.T) {
	h := newSocketHarness(t)
	ours, theirs := net.Pipe()
	defer func() { _ = theirs.Close() }()
	e := &socketEntry{kind: socketTCP, closed: true}
	h.tab.mu.Lock()
	h.tab.attach(e, ours)
	h.tab.mu.Unlock()
	if e.conn != nil {
		t.Error("a closed socket took the connection")
	}
	if !socketPeerClosed(t, theirs) {
		t.Error("the connection stayed open")
	}
}

func TestSocketAddrText(t *testing.T) {
	if got := socketAddrText(nil); got != "" {
		t.Errorf("no address = %q", got)
	}
	// A peer's path that PHP never gave is named as the default mapping
	// would, which leaves this one alone on every host.
	if got := socketAddrText(&net.UnixAddr{Name: "/peer.sock", Net: "unix"}); got != "/peer.sock" {
		t.Errorf("peer path = %q", got)
	}
}

func TestSocketUnixStream(t *testing.T) {
	path := socketShortTempDir(t) + "/s.sock"
	h := newSocketHarness(t)
	h.open(1, socketUnix)
	if errno := h.bind(1, path); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	if errno := h.x.listen(h.ctx, 1, 1); errno != 0 {
		t.Fatalf("listen = %d", errno)
	}
	if name, _ := h.name(1, 0, 256); name != path {
		t.Errorf("listener name = %q, want %q", name, path)
	}
	h.open(2, socketUnix)
	if errno := h.connect(2, path, 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if peer, _ := h.name(2, 1, 256); peer != path {
		t.Errorf("peer name = %q, want %q", peer, path)
	}
	if errno := h.x.accept(h.ctx, 1, 3, 0); errno != 0 {
		t.Fatalf("accept = %d", errno)
	}
	if n := h.send(2, "over unix", "", 0); n != 9 {
		t.Errorf("send = %d", n)
	}
	if got := h.recvAll(3, 9); got != "over unix" {
		t.Errorf("recv = %q", got)
	}
}

func TestSocketCloseOne(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if errno := h.x.close(h.ctx, 1); errno != 0 {
		t.Errorf("close = %d", errno)
	}
	if h.entry(1) != nil {
		t.Error("the socket is still there")
	}
}

// An accept that the table's Close wakes reports EBADF.
func TestSocketAcceptOnClosedSocket(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	if errno := h.x.listen(h.ctx, 1, 1); errno != 0 {
		t.Fatalf("listen = %d", errno)
	}
	// Closed but still in the table, as a waiting accept sees it.
	h.tab.mu.Lock()
	h.tab.entries[1].close()
	h.tab.mu.Unlock()
	if errno := h.x.accept(h.ctx, 1, 2, 1); errno != wasi.EBADF {
		t.Errorf("accept = %d, want EBADF", errno)
	}
}

// A recv that the table's Close wakes, with nothing read, reports ENOTCONN.
func TestSocketRecvOnClosedStream(t *testing.T) {
	h := newSocketHarness(t)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	h.open(1, socketTCP)
	if errno := h.connect(1, l.Addr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	h.tab.mu.Lock()
	h.tab.entries[1].close()
	h.tab.mu.Unlock()
	if _, _, r := h.recv(1, 10, 0); r != -wasi.ENOTCONN {
		t.Errorf("recv = %d, want -ENOTCONN", r)
	}
}

// A stream the host broke reports its error to recv and send.
func TestSocketStreamHostError(t *testing.T) {
	h := newSocketHarness(t)
	h.tab.mu.Lock()
	h.tab.entries[1] = &socketEntry{kind: socketUnix, conn: socketBrokenConn{}, lazy: true}
	h.tab.mu.Unlock()
	if _, _, r := h.recv(1, 10, 0); r != -wasi.EIO {
		t.Errorf("recv = %d, want -EIO", r)
	}
	if n := h.send(1, "x", "", 0); n != -wasi.EIO {
		t.Errorf("send = %d, want -EIO", n)
	}
	if ev := h.entry(1).events(socketPollIn); ev != socketPollIn|socketPollErr {
		t.Errorf("events = %d, want IN|ERR", ev)
	}
}

func TestSocketConnectedDatagramSendFails(t *testing.T) {
	h := newSocketHarness(t)
	server := socketEchoPacket(t)
	h.open(1, socketUDP)
	if errno := h.connect(1, server.LocalAddr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if err := h.entry(1).conn.Close(); err != nil {
		t.Fatal(err)
	}
	if n := h.send(1, "x", "", 0); n != -wasi.EBADF {
		t.Errorf("send on a closed connection = %d, want -EBADF", n)
	}
}

// sendto on a Unix datagram socket maps the destination as connect does.
func TestSocketUnixgramSendtoUnmapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix datagram sockets")
	}
	dir := socketShortTempDir(t)
	h := newSocketHarness(t)
	h.tab.hostPath = func(path string) (string, bool, bool) {
		return path, true, strings.HasPrefix(path, dir)
	}
	h.open(1, socketUnixgram)
	if errno := h.bind(1, dir+"/s.sock"); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	if n := h.send(1, "x", "/elsewhere.sock", 0); n != -wasi.ENOENT {
		t.Errorf("sendto an unmapped path = %d, want -ENOENT", n)
	}
}

// A blocking send waits for a connect under way, and an interrupt wakes it.
func TestSocketSendInterruptedWhileConnecting(t *testing.T) {
	h := newSocketHarness(t)
	h.open(1, socketTCP)
	h.tab.mu.Lock()
	h.tab.entries[1].connecting = true
	h.tab.mu.Unlock()
	close(h.run.intr)
	if n := h.send(1, "x", "", 0); n != -wasi.EINTR {
		t.Errorf("send = %d, want -EINTR", n)
	}
}

// A connection the host cannot duplicate is not given to a child.
func TestSocketChildFileOfClosedConn(t *testing.T) {
	h := newSocketHarness(t)
	l := socketEchoListener(t, "tcp", "127.0.0.1:0")
	h.open(1, socketTCP)
	if errno := h.connect(1, l.Addr().String(), 0); errno != 0 {
		t.Fatalf("connect = %d", errno)
	}
	if err := h.entry(1).conn.Close(); err != nil {
		t.Fatal(err)
	}
	if f, _, err := h.tab.ChildFile(1); err == nil {
		_ = f.Close() // The test fails already; the file is only released.
		t.Error("ChildFile of a closed connection succeeded")
	}
}

func TestSocketPoll(t *testing.T) {
	h := newSocketHarness(t)
	const in, out, inOut = socketPollIn, socketPollOut, socketPollIn | socketPollOut

	// An unknown fd is an error, and an unconnected stream has hung up.
	h.open(1, socketTCP)
	if got, n := socketPoll(h, []int32{99, 1}, []int32{in, inOut}, 0); n != 2 || got[0] != socketPollErr || got[1] != socketPollHup {
		t.Errorf("poll = %v, %d", got, n)
	}

	// A write end is writable at once, even with no timeout. The read end
	// starts reading ahead once polled for input.
	if errno := h.x.pipeOpen(h.ctx, 2, 3); errno != 0 {
		t.Fatalf("pipe_open = %d", errno)
	}
	if got, n := socketPoll(h, []int32{2, 3}, []int32{in, out}, -1); n != 1 || got[0] != 0 || got[1] != out {
		t.Errorf("poll = %v, %d", got, n)
	}
	if !h.entry(2).reading {
		t.Error("polling for input did not start reading")
	}
	if got, n := socketPoll(h, []int32{2}, []int32{in}, 0); n != 0 || got[0] != 0 {
		t.Errorf("poll with nothing to read = %v, %d", got, n)
	}
	if n := h.send(3, "x", "", 0); n != 1 {
		t.Fatalf("send = %d", n)
	}
	if got, n := socketPoll(h, []int32{2}, []int32{in}, 2000); n != 1 || got[0] != in {
		t.Errorf("poll after a write = %v, %d", got, n)
	}

	// A listener is readable once a connection waits.
	h.open(4, socketTCP)
	if errno := h.x.listen(h.ctx, 4, 1); errno != 0 {
		t.Fatalf("listen = %d", errno)
	}
	addr, _ := h.name(4, 0, 64)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if got, n := socketPoll(h, []int32{4}, []int32{inOut}, 2000); n != 1 || got[0] != in {
		t.Errorf("poll a listener = %v, %d", got, n)
	}

	// A datagram socket is always writable, and readable with a datagram.
	h.open(5, socketUDP)
	if errno := h.bind(5, "127.0.0.1:0"); errno != 0 {
		t.Fatalf("bind = %d", errno)
	}
	self, _ := h.name(5, 0, 64)
	if got, n := socketPoll(h, []int32{5}, []int32{inOut}, 0); n != 1 || got[0] != out {
		t.Errorf("poll an empty datagram socket = %v, %d", got, n)
	}
	if n := h.send(5, "self", self, 0); n != 4 {
		t.Fatalf("sendto = %d", n)
	}
	h.await(5, func(e *socketEntry) bool { return len(e.packets) > 0 })
	if got, n := socketPoll(h, []int32{5}, []int32{inOut}, 0); n != 1 || got[0] != inOut {
		t.Errorf("poll a datagram socket with a datagram = %v, %d", got, n)
	}

	// A failed connect is readable, writable, hung up and in error.
	h.open(6, socketTCP)
	if errno := h.connect(6, socketClosedPort(t), 1); errno != wasi.EINPROGRESS {
		t.Fatalf("connect = %d", errno)
	}
	// Polled rather than awaited: Windows retries a refused connect for
	// about two seconds, and poll reports nothing while it is under way.
	if got, n := socketPoll(h, []int32{6}, []int32{inOut}, 10000); n != 1 || got[0] != inOut|socketPollHup|socketPollErr {
		t.Errorf("poll a failed connect = %v, %d", got, n)
	}

	// A wait wakes on an interrupt, and gives up once the run is over.
	if errno := h.x.pipeOpen(h.ctx, 7, 8); errno != 0 {
		t.Fatalf("pipe_open = %d", errno)
	}
	close(h.run.intr)
	if _, n := socketPoll(h, []int32{7}, []int32{in}, -1); n != -wasi.EINTR {
		t.Errorf("poll on interrupt = %d, want -EINTR", n)
	}
	h.run.intr = make(chan struct{})
	h.run.cancel()
	if _, n := socketPoll(h, []int32{7}, []int32{in}, -1); n != -wasi.EIO {
		t.Errorf("poll once the run is over = %d, want -EIO", n)
	}
}
