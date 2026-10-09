//go:build !unix

//declscope:namespace socket

package hostnet

import (
	"errors"
	"net"
)

// socketPair fails: socketpair(2) needs a Unix host.
func socketPair() (net.Conn, net.Conn, error) {
	return nil, nil, errors.ErrUnsupported
}
