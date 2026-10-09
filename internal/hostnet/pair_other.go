//go:build !unix

//declscope:namespace socket

package hostnet

import (
	"errors"
	"net"
)

// socketPair connects two loopback TCP sockets, as Windows has no
// socketpair(2). A stream pair behaves the same for PHP.
func socketPair() (net.Conn, net.Conn, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	// Only the one connection below is accepted.
	defer func() { _ = l.Close() }()
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := l.Accept()
		ch <- accepted{c, err}
	}()
	a, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	b := <-ch
	if b.err != nil {
		return nil, nil, errors.Join(b.err, a.Close())
	}
	return a, b.conn, nil
}
