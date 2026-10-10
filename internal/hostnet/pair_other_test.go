//go:build !unix

//declscope:namespace socket

package hostnet

import (
	"errors"
	"io"
	"net"
	"testing"
)

// TestSocketPairAcceptSkipsStrangers takes only the dialer's connection.
// Another local process may connect to the listener first, and is closed.
func TestSocketPairAcceptSkipsStrangers(t *testing.T) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	stranger, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stranger.Close() }()
	peer, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	c, err := socketPairAccept(l, peer.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.RemoteAddr().String() != peer.LocalAddr().String() {
		t.Errorf("accepted %s, want %s", c.RemoteAddr(), peer.LocalAddr())
	}
	// The stranger's end was closed: EOF, or a reset.
	_, err = stranger.Read(make([]byte, 1))
	if _, reset := errors.AsType[*net.OpError](err); !errors.Is(err, io.EOF) && !reset {
		t.Errorf("stranger read: %v", err)
	}
}

// TestSocketPairAcceptFails gives up on a listener that is closed, before
// the deadline is set or while it waits for the dialer.
func TestSocketPairAcceptFails(t *testing.T) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := socketPairAccept(l, "127.0.0.1:1"); err == nil {
		t.Error("a closed listener took a deadline")
	}

	// A stranger is accepted and closed first, so by the time its read
	// ends, the deadline is set and the next Accept waits for the dialer,
	// which never comes. Closing the listener ends that wait.
	l, err = net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }() // closed already, as expected
	stranger, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stranger.Close() }() // its peer closed it
	go func() {
		_, _ = stranger.Read(make([]byte, 1)) // EOF or a reset, once refused
		_ = l.Close()                         // the error goes to the Accept below
	}()
	if _, err := socketPairAccept(l, "127.0.0.1:1"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("closed while waiting: %v", err)
	}
}
