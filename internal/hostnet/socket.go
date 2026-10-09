package hostnet

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"sync"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Sockets for PHP. gophper-wasm's compat/gophper_net.c routes every socket call on the
// guest side to the host functions here, and each socket is a Go net.Conn,
// net.Listener or net.PacketConn. See that file for why.
//
// A goroutine per socket reads ahead into a buffer, so recv and poll only
// look at buffered state. Every change to any socket of an instance closes
// socketTable.changed, which is what poll and blocking calls wait on, along
// with the instance's interrupts.

// Kinds, message flags, poll bits and options. Keep in sync with gophper-wasm (ABI.md).
const (
	socketTCP      int32 = 1
	socketUDP      int32 = 2
	socketUnix     int32 = 3
	socketUnixgram int32 = 4

	socketMsgPeek     int32 = 1
	socketMsgDontwait int32 = 2

	socketPollIn  int32 = 1
	socketPollOut int32 = 2
	socketPollHup int32 = 4
	socketPollErr int32 = 8

	socketOptError     int32 = 1
	socketOptType      int32 = 2
	socketOptNodelay   int32 = 3
	socketOptKeepalive int32 = 4
	socketOptReuseaddr int32 = 5
	socketOptBroadcast int32 = 6
	socketOptRcvbuf    int32 = 7
	socketOptSndbuf    int32 = 8
)

// socketMaxBuffered is how much a stream reads ahead before it waits for PHP.
const socketMaxBuffered = 1 << 20

// ExportSockets adds the socket host functions to b, the "gophper" host
// module. from returns the sockets of the instance a call comes from.
func ExportSockets(b wazero.HostModuleBuilder, from func(context.Context) *Sockets) {
	x := socketExports{from: from}
	fns := map[string]any{
		"sock_open":      x.open,
		"sock_close":     x.close,
		"sock_connect":   x.connect,
		"sock_bind":      x.bind,
		"sock_listen":    x.listen,
		"sock_accept":    x.accept,
		"sock_recv":      x.recv,
		"sock_send":      x.send,
		"sock_shutdown":  x.shutdown,
		"sock_name":      x.name,
		"sock_getopt":    x.getopt,
		"sock_setopt":    x.setopt,
		"sock_poll":      x.poll,
		"sock_available": x.available,
		"pipe_open":      x.pipeOpen,
		"sock_pair":      x.pair,
	}
	for name, fn := range fns {
		b.NewFunctionBuilder().WithFunc(fn).Export(name)
	}
}

// SocketPlaceholderDir is where SocketPlaceholderFS must be mounted.
// gophper-wasm's compat/gophper_net.c opens SocketPlaceholderDir/socket to reserve a
// WASI fd number for each socket.
const SocketPlaceholderDir = "/.gophper"

// SocketPlaceholderFS holds the empty file that reserves socket fds.
var SocketPlaceholderFS fs.FS = socketPlaceholderFS{}

type socketPlaceholderFS struct{}

func (socketPlaceholderFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return socketPlaceholderFile{dir: true}, nil
	case "socket":
		return socketPlaceholderFile{}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Sockets holds the sockets of one PHP instance, keyed by guest fd.
type Sockets struct {
	run     wasi.Run
	mu      sync.Mutex
	entries map[int32]*socketEntry
	// changed is closed on any change to any socket, then replaced.
	changed chan struct{}
}

// NewSockets returns an empty table for one PHP instance.
func NewSockets(run wasi.Run) *Sockets {
	return &Sockets{run: run, entries: map[int32]*socketEntry{}, changed: make(chan struct{})}
}

// Close closes every socket the script left open.
func (t *Sockets) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for fd, e := range t.entries {
		e.close()
		delete(t.entries, fd)
	}
	t.notify()
}

// notify wakes everything waiting on t.changed. t.mu must be held.
func (t *Sockets) notify() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// wait blocks until ready holds, an interrupt, or the timeout. A negative
// timeout waits forever. t.mu must be held, and is released while waiting.
//
// It returns 0, EAGAIN for the timeout, EINTR for an interrupt, or EIO once
// the run's context is done. PHP retries a call that fails with EINTR, as it
// would after a signal. After the context is done, retrying would spin
// forever, so EIO makes PHP give up and return to the VM, which then stops.
func (t *Sockets) wait(ready func() bool, timeout time.Duration) int32 {
	var timer <-chan time.Time
	if timeout >= 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		timer = tm.C
	}
	intr := t.run.Interruption()
	for !ready() {
		changed := t.changed
		t.mu.Unlock()
		select {
		case <-changed:
			t.mu.Lock()
		case <-intr:
			t.mu.Lock()
			return wasi.EINTR
		case <-t.run.Context().Done():
			t.mu.Lock()
			return wasi.EIO
		case <-timer:
			t.mu.Lock()
			if ready() {
				return 0
			}
			return wasi.EAGAIN
		}
	}
	return 0
}

func (t *Sockets) readStream(e *socketEntry, conn net.Conn) {
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		t.mu.Lock()
		e.recvBuf = append(e.recvBuf, buf[:n]...)
		if err != nil && !e.closed {
			e.recvErr = err
		}
		t.notify()
		for err == nil && !e.closed && len(e.recvBuf) >= socketMaxBuffered {
			changed := t.changed
			t.mu.Unlock()
			<-changed
			t.mu.Lock()
		}
		stop := err != nil || e.closed
		t.mu.Unlock()
		if stop {
			return
		}
	}
}

// readPackets queues datagrams. A connected datagram socket reads through
// conn, and its sender is always the peer.
func (t *Sockets) readPackets(e *socketEntry, pc net.PacketConn, conn net.Conn) {
	buf := make([]byte, 64<<10)
	for {
		var n int
		var from net.Addr
		var err error
		if pc != nil {
			n, from, err = pc.ReadFrom(buf)
		} else {
			n, err = conn.Read(buf)
			from = conn.RemoteAddr()
		}
		t.mu.Lock()
		if err == nil {
			e.packets = append(e.packets, socketPacket{data: append([]byte(nil), buf[:n]...), from: socketAddrText(from)})
			// Like a full socket buffer, drop the oldest rather than block.
			if len(e.packets) > 256 {
				e.packets = e.packets[1:]
			}
		}
		t.notify()
		stop := err != nil || e.closed
		t.mu.Unlock()
		if stop {
			return
		}
	}
}

func (t *Sockets) acceptLoop(e *socketEntry, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		t.mu.Lock()
		if err != nil {
			if !e.closed {
				e.acceptErr = err
			}
		} else if e.closed {
			// Nobody will accept it. The peer sees the close, and there is
			// nobody to tell about a failure.
			_ = conn.Close()
		} else {
			e.pending = append(e.pending, conn)
		}
		t.notify()
		stop := err != nil || e.closed
		t.mu.Unlock()
		if stop {
			return
		}
	}
}

// attach makes conn the socket's stream connection. t.mu must be held.
func (t *Sockets) attach(e *socketEntry, conn net.Conn) {
	if e.closed {
		// The guest closed the socket already, so nobody can see a failure.
		_ = conn.Close()
		return
	}
	e.conn = conn
	e.reading = true
	if tc, ok := conn.(*net.TCPConn); ok {
		// Native sockets start with Nagle on. Go turns it off by default.
		// The connect or accept itself succeeded, and a non-blocking connect
		// has nobody to report to, so a failure here is ignored. It means the
		// socket is broken, which its next read or write reports.
		_ = tc.SetNoDelay(e.nodelay)
		if e.keepalive {
			_ = tc.SetKeepAlive(true)
		}
	}
	go t.readStream(e, conn)
}

// socketEntry is one socket. All fields are guarded by socketTable.mu.
type socketEntry struct {
	kind int32
	// bound is the address given to bind(2).
	bound string

	// conn is a connected stream, or a connected datagram socket.
	conn       net.Conn
	connecting bool
	shutWrite  bool
	recvBuf    []byte
	// recvErr is why the stream ended. io.EOF means an orderly close.
	recvErr error

	listener  net.Listener
	pending   []net.Conn
	acceptErr error

	// packet is a bound, or implicitly bound, datagram socket.
	packet  net.PacketConn
	packets []socketPacket

	// soError is SO_ERROR: a failed non-blocking connect, cleared once read.
	soError int32

	// pipe is set for an end of a pipe(2).
	pipe socketPipeEnd
	// lazy is set for a pipe or socketpair(2) end. Nothing reads it ahead
	// until PHP reads or polls it: the end may be for a child process, and
	// reading ahead would take its input.
	lazy    bool
	reading bool

	nodelay, keepalive, reuseaddr, broadcast bool
	rcvbuf, sndbuf                           int32

	closed bool
}

func (e *socketEntry) datagram() bool {
	return e.kind == socketUDP || e.kind == socketUnixgram
}

func (e *socketEntry) network() string {
	switch e.kind {
	case socketTCP:
		return "tcp"
	case socketUDP:
		return "udp"
	case socketUnix:
		return "unix"
	}
	return "unixgram"
}

// close releases the host resources. Their Close errors are ignored: like
// close(2) on a socket, they report nothing the guest can act on, and the fd
// is gone either way.
func (e *socketEntry) close() {
	e.closed = true
	if e.conn != nil {
		_ = e.conn.Close()
	}
	if e.listener != nil {
		_ = e.listener.Close()
	}
	if e.packet != nil {
		_ = e.packet.Close()
	}
	for _, c := range e.pending {
		_ = c.Close()
	}
	e.pending = nil
}

// events reports which of the poll bits in want are ready.
func (e *socketEntry) events(want int32) int32 {
	var in, out bool
	var r int32
	switch {
	case e.listener != nil:
		in = len(e.pending) > 0 || e.acceptErr != nil
	case e.datagram():
		in = len(e.packets) > 0
		out = true
	case e.pipe == socketPipeWrite:
		out = !e.closed
	default:
		failed := !e.connecting && e.conn == nil && e.soError != 0
		in = len(e.recvBuf) > 0 || e.recvErr != nil || failed
		out = (e.conn != nil && !e.shutWrite) || failed
		if errors.Is(e.recvErr, io.EOF) || (!e.connecting && e.conn == nil) {
			r |= socketPollHup
		}
		if e.soError != 0 || (e.recvErr != nil && !errors.Is(e.recvErr, io.EOF)) {
			r |= socketPollErr
		}
	}
	if in && want&socketPollIn != 0 {
		r |= socketPollIn
	}
	if out && want&socketPollOut != 0 {
		r |= socketPollOut
	}
	return r
}

type socketPacket struct {
	data []byte
	from string
}

// socketAddrText formats an address as gophper-wasm's compat/gophper_net.c parses it.
func socketAddrText(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

// socketExports are the host functions. Each returns a WASI errno, or for
// the int32 counts, -errno.
type socketExports struct {
	from func(context.Context) *Sockets
}

func socketString(m api.Module, ptr, n uint32) string {
	b, _ := m.Memory().Read(ptr, n)
	return string(b)
}

func (x socketExports) open(ctx context.Context, fd, kind int32) int32 {
	t := x.from(ctx)
	if kind < socketTCP || kind > socketUnixgram {
		return wasi.EPROTONOSUPPORT
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[fd] = &socketEntry{kind: kind, rcvbuf: 65536, sndbuf: 65536}
	return 0
}

func (x socketExports) close(ctx context.Context, fd int32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[fd]; ok {
		e.close()
		delete(t.entries, fd)
		t.notify()
	}
	return 0
}

func (x socketExports) connect(ctx context.Context, m api.Module, fd int32, addrPtr, addrLen uint32, nonblock int32) int32 {
	t := x.from(ctx)
	addr := socketString(m, addrPtr, addrLen)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	switch {
	case e == nil:
		return wasi.EBADF
	case e.conn != nil:
		return wasi.EISCONN
	case e.connecting:
		return wasi.EALREADY
	case e.listener != nil:
		return wasi.EINVAL
	}

	if e.datagram() {
		// No handshake: connecting only fixes the peer.
		conn, err := net.Dial(e.network(), addr)
		if err != nil {
			return errnoFrom(err)
		}
		e.conn = conn
		go t.readPackets(e, nil, conn)
		return 0
	}

	d := net.Dialer{}
	if e.bound != "" && e.kind == socketTCP {
		if la, err := net.ResolveTCPAddr("tcp", e.bound); err == nil {
			d.LocalAddr = la
		}
	}
	network := e.network()
	e.connecting = true

	if nonblock != 0 {
		go func() {
			conn, err := d.DialContext(t.run.Context(), network, addr)
			t.mu.Lock()
			defer t.mu.Unlock()
			e.connecting = false
			if err != nil {
				e.soError = errnoFrom(err)
			} else {
				t.attach(e, conn)
			}
			t.notify()
		}()
		return wasi.EINPROGRESS
	}

	dctx, cancel := interruptibleRunContext(t.run)
	t.mu.Unlock()
	conn, err := d.DialContext(dctx, network, addr)
	interrupted := dctx.Err() != nil
	cancel()
	t.mu.Lock()
	e.connecting = false
	t.notify()
	if err != nil {
		if interrupted {
			return wasi.EINTR
		}
		return errnoFrom(err)
	}
	t.attach(e, conn)
	return 0
}

func (x socketExports) bind(ctx context.Context, m api.Module, fd int32, addrPtr, addrLen uint32) int32 {
	t := x.from(ctx)
	addr := socketString(m, addrPtr, addrLen)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return wasi.EBADF
	}
	if e.bound != "" || e.conn != nil {
		return wasi.EINVAL
	}
	if e.datagram() {
		pc, err := net.ListenPacket(e.network(), addr)
		if err != nil {
			return errnoFrom(err)
		}
		e.packet = pc
		e.bound = pc.LocalAddr().String()
		go t.readPackets(e, pc, nil)
		return 0
	}
	e.bound = addr
	return 0
}

func (x socketExports) listen(ctx context.Context, fd, backlog int32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	switch {
	case e == nil:
		return wasi.EBADF
	case e.datagram():
		return wasi.ENOTSUP
	case e.listener != nil:
		return 0
	}
	addr := e.bound
	if addr == "" {
		if e.kind == socketUnix {
			return wasi.EINVAL
		}
		addr = ":0"
	}
	ln, err := (&net.ListenConfig{}).Listen(t.run.Context(), e.network(), addr)
	if err != nil {
		return errnoFrom(err)
	}
	e.listener = ln
	go t.acceptLoop(e, ln)
	return 0
}

func (x socketExports) accept(ctx context.Context, fd, newFD, nonblock int32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil || e.listener == nil {
		return wasi.EINVAL
	}
	timeout := time.Duration(-1)
	if nonblock != 0 {
		timeout = 0
	}
	if errno := t.wait(func() bool { return len(e.pending) > 0 || e.acceptErr != nil || e.closed }, timeout); errno != 0 {
		return errno
	}
	if len(e.pending) == 0 {
		if e.acceptErr != nil {
			return errnoFrom(e.acceptErr)
		}
		return wasi.EBADF
	}
	conn := e.pending[0]
	e.pending = e.pending[1:]
	ne := &socketEntry{kind: e.kind, rcvbuf: e.rcvbuf, sndbuf: e.sndbuf, nodelay: e.nodelay, keepalive: e.keepalive}
	t.entries[newFD] = ne
	t.attach(ne, conn)
	return 0
}

func (x socketExports) recv(ctx context.Context, m api.Module, fd int32, bufPtr uint32, n int32, flags int32, fromPtr, fromCap, fromLenPtr uint32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return -wasi.EBADF
	}
	timeout := time.Duration(-1)
	if flags&socketMsgDontwait != 0 {
		timeout = 0
	}
	peek := flags&socketMsgPeek != 0

	if e.datagram() {
		if e.conn == nil && e.packet == nil {
			return -wasi.ENOTCONN
		}
		if errno := t.wait(func() bool { return len(e.packets) > 0 || e.closed }, timeout); errno != 0 {
			return -errno
		}
		if len(e.packets) == 0 {
			return -wasi.EBADF
		}
		p := e.packets[0]
		if !peek {
			e.packets = e.packets[1:]
		}
		size := min(len(p.data), int(n))
		m.Memory().Write(bufPtr, p.data[:size])
		socketWriteText(m, p.from, fromPtr, fromCap, fromLenPtr)
		return int32(size)
	}

	if e.conn == nil && !e.connecting {
		if e.soError != 0 {
			return -e.soError
		}
		return -wasi.ENOTCONN
	}
	if e.pipe == socketPipeWrite {
		return -wasi.EBADF
	}
	t.startReading(e)
	ready := func() bool {
		return len(e.recvBuf) > 0 || e.recvErr != nil || e.closed || (!e.connecting && e.conn == nil)
	}
	if errno := t.wait(ready, timeout); errno != 0 {
		return -errno
	}
	if len(e.recvBuf) > 0 {
		size := min(len(e.recvBuf), int(n))
		m.Memory().Write(bufPtr, e.recvBuf[:size])
		if !peek {
			e.recvBuf = append(e.recvBuf[:0:0], e.recvBuf[size:]...)
			t.notify()
		}
		if e.conn != nil {
			socketWriteText(m, socketAddrText(e.conn.RemoteAddr()), fromPtr, fromCap, fromLenPtr)
		}
		return int32(size)
	}
	switch {
	case e.recvErr == nil:
		return -wasi.ENOTCONN
	case errors.Is(e.recvErr, io.EOF):
		return 0
	}
	return -errnoFrom(e.recvErr)
}

func socketWriteText(m api.Module, text string, ptr, capacity, lenPtr uint32) {
	if lenPtr == 0 {
		return
	}
	if len(text) > int(capacity) {
		text = text[:capacity]
	}
	m.Memory().WriteString(ptr, text)
	m.Memory().WriteUint32Le(lenPtr, uint32(len(text)))
}

func (x socketExports) send(ctx context.Context, m api.Module, fd int32, bufPtr uint32, n int32, flags int32, toPtr, toLen uint32) int32 {
	t := x.from(ctx)
	data, ok := m.Memory().Read(bufPtr, uint32(n))
	if !ok {
		return -wasi.EINVAL
	}
	data = append([]byte(nil), data...)
	to := ""
	if toLen > 0 {
		to = socketString(m, toPtr, toLen)
	}

	t.mu.Lock()
	e := t.entries[fd]
	if e == nil {
		t.mu.Unlock()
		return -wasi.EBADF
	}

	if e.datagram() {
		if e.conn != nil && to != "" && to != socketAddrText(e.conn.RemoteAddr()) {
			// As on BSD, a connected socket sends only to its peer. Sending
			// through e.packet would come from another port.
			t.mu.Unlock()
			return -wasi.EISCONN
		}
		if to == "" || e.conn != nil {
			conn := e.conn
			t.mu.Unlock()
			if conn == nil {
				return -wasi.EDESTADDRREQ
			}
			written, err := conn.Write(data)
			if err != nil {
				return -errnoFrom(err)
			}
			return int32(written)
		}
		if e.packet == nil {
			if e.kind != socketUDP {
				t.mu.Unlock()
				return -wasi.ENOTSUP
			}
			// sendto(2) on an unbound socket binds it to an ephemeral port.
			pc, err := net.ListenPacket("udp", ":0")
			if err != nil {
				t.mu.Unlock()
				return -errnoFrom(err)
			}
			e.packet = pc
			go t.readPackets(e, pc, nil)
		}
		pc, kind := e.packet, e.kind
		t.mu.Unlock()
		var raddr net.Addr
		if kind == socketUDP {
			ua, err := net.ResolveUDPAddr("udp", to)
			if err != nil {
				return -errnoFrom(err)
			}
			raddr = ua
		} else {
			raddr = &net.UnixAddr{Name: to, Net: "unixgram"}
		}
		written, err := pc.WriteTo(data, raddr)
		if err != nil {
			return -errnoFrom(err)
		}
		return int32(written)
	}

	if e.connecting {
		if flags&socketMsgDontwait != 0 {
			t.mu.Unlock()
			return -wasi.EAGAIN
		}
		if errno := t.wait(func() bool { return !e.connecting || e.closed }, -1); errno != 0 {
			t.mu.Unlock()
			return -errno
		}
	}
	conn, shut, soErr := e.conn, e.shutWrite, e.soError
	t.mu.Unlock()
	switch {
	case conn == nil && soErr != 0:
		return -soErr
	case conn == nil:
		return -wasi.ENOTCONN
	case shut:
		return -wasi.EPIPE
	}
	written, err := conn.Write(data)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return -wasi.EPIPE
		}
		return -errnoFrom(err)
	}
	return int32(written)
}

func (x socketExports) shutdown(ctx context.Context, fd, how int32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return wasi.EBADF
	}
	if e.conn == nil {
		return wasi.ENOTCONN
	}
	half, ok := e.conn.(interface {
		CloseRead() error
		CloseWrite() error
	})
	if !ok {
		return 0
	}
	if how&1 != 0 {
		if err := half.CloseRead(); err != nil {
			return errnoFrom(err)
		}
	}
	if how&2 != 0 {
		if err := half.CloseWrite(); err != nil {
			return errnoFrom(err)
		}
		e.shutWrite = true
	}
	t.notify()
	return 0
}

func (x socketExports) name(ctx context.Context, m api.Module, fd, peer int32, outPtr, outCap, outLenPtr uint32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return wasi.EBADF
	}
	var text string
	switch {
	case peer != 0 && e.conn == nil:
		return wasi.ENOTCONN
	case peer != 0:
		text = socketAddrText(e.conn.RemoteAddr())
	case e.conn != nil:
		text = socketAddrText(e.conn.LocalAddr())
	case e.listener != nil:
		text = socketAddrText(e.listener.Addr())
	case e.packet != nil:
		text = socketAddrText(e.packet.LocalAddr())
	case e.bound != "":
		text = e.bound
	case e.kind == socketTCP || e.kind == socketUDP:
		text = "0.0.0.0:0"
	}
	socketWriteText(m, text, outPtr, outCap, outLenPtr)
	return 0
}

func (x socketExports) getopt(ctx context.Context, m api.Module, fd, opt int32, valuePtr uint32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return wasi.EBADF
	}
	b := func(v bool) int32 {
		if v {
			return 1
		}
		return 0
	}
	var v int32
	switch opt {
	case socketOptError:
		v, e.soError = e.soError, 0
	case socketOptNodelay:
		v = b(e.nodelay)
	case socketOptKeepalive:
		v = b(e.keepalive)
	case socketOptReuseaddr:
		v = b(e.reuseaddr)
	case socketOptBroadcast:
		v = b(e.broadcast)
	case socketOptRcvbuf:
		v = e.rcvbuf
	case socketOptSndbuf:
		v = e.sndbuf
	default:
		return wasi.EINVAL
	}
	m.Memory().WriteUint32Le(valuePtr, uint32(v))
	return 0
}

func (x socketExports) setopt(ctx context.Context, fd, opt, value int32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return wasi.EBADF
	}
	// On a connected socket, the host's option is set first. If that fails,
	// the option keeps its old value, as with setsockopt(2).
	tc, _ := e.conn.(*net.TCPConn)
	on := value != 0
	switch opt {
	case socketOptNodelay:
		if tc != nil {
			if err := tc.SetNoDelay(on); err != nil {
				return errnoFrom(err)
			}
		}
		e.nodelay = on
	case socketOptKeepalive:
		if tc != nil {
			if err := tc.SetKeepAlive(on); err != nil {
				return errnoFrom(err)
			}
		}
		e.keepalive = on
	case socketOptReuseaddr:
		// Go listeners set SO_REUSEADDR already.
		e.reuseaddr = on
	case socketOptBroadcast:
		e.broadcast = on
	case socketOptRcvbuf:
		if tc != nil {
			if err := tc.SetReadBuffer(int(value)); err != nil {
				return errnoFrom(err)
			}
		}
		e.rcvbuf = value
	case socketOptSndbuf:
		if tc != nil {
			if err := tc.SetWriteBuffer(int(value)); err != nil {
				return errnoFrom(err)
			}
		}
		e.sndbuf = value
	default:
		return wasi.EINVAL
	}
	return 0
}

func (x socketExports) poll(ctx context.Context, m api.Module, fdsPtr, eventsPtr, reventsPtr uint32, n, timeoutMs int32) int32 {
	t := x.from(ctx)
	mem := m.Memory()
	fds := make([]int32, n)
	want := make([]int32, n)
	for i := range n {
		f, _ := mem.ReadUint32Le(fdsPtr + uint32(i)*4)
		w, _ := mem.ReadUint32Le(eventsPtr + uint32(i)*4)
		fds[i], want[i] = int32(f), int32(w)
	}
	got := make([]int32, n)

	t.mu.Lock()
	defer t.mu.Unlock()
	count := int32(0)
	scan := func() bool {
		count = 0
		for i, fd := range fds {
			if e := t.entries[fd]; e != nil {
				if want[i]&socketPollIn != 0 {
					t.startReading(e)
				}
				got[i] = e.events(want[i])
			} else {
				got[i] = socketPollErr
			}
			if got[i] != 0 {
				count++
			}
		}
		return count > 0
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeoutMs < 0 {
		timeout = -1
	}
	if errno := t.wait(scan, timeout); errno == wasi.EINTR || errno == wasi.EIO {
		return -errno
	}
	for i := range got {
		mem.WriteUint32Le(reventsPtr+uint32(i)*4, uint32(got[i]))
	}
	return count
}

func (x socketExports) available(ctx context.Context, fd int32) int32 {
	t := x.from(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[fd]
	if e == nil {
		return -wasi.EBADF
	}
	if e.datagram() {
		if len(e.packets) > 0 {
			return int32(len(e.packets[0].data))
		}
		return 0
	}
	t.startReading(e)
	return int32(len(e.recvBuf))
}

// socketPlaceholderFile is the empty file, or the directory holding it.
type socketPlaceholderFile struct{ dir bool }

func (f socketPlaceholderFile) Stat() (fs.FileInfo, error) { return f, nil }
func (socketPlaceholderFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (socketPlaceholderFile) Close() error                 { return nil }
func (socketPlaceholderFile) ReadDir(int) ([]fs.DirEntry, error) {
	return nil, nil
}

func (f socketPlaceholderFile) Name() string {
	if f.dir {
		return "."
	}
	return "socket"
}
func (socketPlaceholderFile) Size() int64 { return 0 }
func (f socketPlaceholderFile) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (socketPlaceholderFile) ModTime() time.Time { return time.Time{} }
func (f socketPlaceholderFile) IsDir() bool      { return f.dir }
func (socketPlaceholderFile) Sys() any           { return nil }
