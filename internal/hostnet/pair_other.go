//go:build !unix

//declscope:namespace socket

package hostnet

import (
	"errors"
	"net"
	"time"
)

// socketPair connects two loopback TCP sockets, as Windows has no
// socketpair(2). A stream pair behaves the same for PHP.
func socketPair() (net.Conn, net.Conn, error) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, nil, err
	}
	// Only the one connection below is accepted.
	defer func() { _ = l.Close() }()
	a, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	b, err := socketPairAccept(l, a.LocalAddr().String())
	if err != nil {
		return nil, nil, errors.Join(err, a.Close())
	}
	return a, b, nil
}

// socketPairAccept accepts the connection from peer. Any local process can
// connect to the listener first, so the others are closed.
func socketPairAccept(l *net.TCPListener, peer string) (net.Conn, error) {
	if err := l.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return nil, err
		}
		if c.RemoteAddr().String() == peer {
			return c, nil
		}
		// A stranger's connection: nothing was sent on it.
		_ = c.Close()
	}
}
