//declscope:namespace dns

package hostnet

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// res_search(3) results, as h_errno values. Keep in sync with gophper-wasm (ABI.md).
const (
	dnsHostNotFound int32 = 1
	dnsTryAgain     int32 = 2
	dnsNoRecovery   int32 = 3
	dnsNoData       int32 = 4
)

// dnsTimeout is how long one server gets for one attempt.
const dnsTimeout = 5 * time.Second

// query sends a raw DNS query, as res_search(3) does, and writes the raw
// answer. PHP's dns_get_record() parses it itself. It returns the length of
// the answer, cut to outCap, or -h_errno.
//
// Go's resolver offers no raw queries, so the packet is built here and
// sent to the host's nameservers.
func (x dnsExports) query(ctx context.Context, m api.Module, namePtr, nameLen uint32, class, typ int32, outPtr, outCap uint32) int32 {
	b, _ := m.Memory().Read(namePtr, nameLen)
	q, id, err := dnsBuildQuery(string(b), uint16(class), uint16(typ))
	if err != nil {
		return -dnsNoRecovery
	}
	run, ok := x.run(ctx)
	if !ok {
		return -dnsNoRecovery
	}
	qctx, cancel := interruptibleRunContext(run)
	defer cancel()

	var answer []byte
	for _, server := range dnsServers() {
		if answer, err = dnsExchange(qctx, server, q, id); err == nil {
			break
		}
		if qctx.Err() != nil {
			return -dnsTryAgain
		}
	}
	if answer == nil {
		return -dnsTryAgain
	}
	switch answer[3] & 0x0f {
	case 0:
	case 3:
		return -dnsHostNotFound
	case 2:
		return -dnsTryAgain
	default:
		return -dnsNoRecovery
	}
	if binary.BigEndian.Uint16(answer[6:8]) == 0 {
		return -dnsNoData
	}
	n := min(len(answer), int(outCap))
	m.Memory().Write(outPtr, answer[:n])
	return int32(n)
}

// dnsBuildQuery returns a query packet with recursion desired, and its id.
func dnsBuildQuery(name string, class, typ uint16) ([]byte, uint16, error) {
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(idb[:])
	p := binary.BigEndian.AppendUint16(nil, id)
	p = append(p, 0x01, 0x00) // RD
	p = binary.BigEndian.AppendUint16(p, 1)
	p = append(p, 0, 0, 0, 0, 0, 0)
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, 0, errors.New("hostnet: bad DNS name")
		}
		p = append(p, byte(len(label)))
		p = append(p, label...)
	}
	p = append(p, 0)
	// At most 255 bytes on the wire (RFC 1035). A longer name would also
	// overflow the two-byte length of a query over TCP.
	if len(p)-12 > 255 {
		return nil, 0, errors.New("hostnet: DNS name too long")
	}
	p = binary.BigEndian.AppendUint16(p, typ)
	p = binary.BigEndian.AppendUint16(p, class)
	return p, id, nil
}

// dnsDialTCP is a var so that a test can give dnsExchange a connection
// that the server reset as it was made, which no test server can time.
var dnsDialTCP = func(ctx context.Context, d *net.Dialer, address string) (net.Conn, error) {
	return d.DialContext(ctx, "tcp", address)
}

// dnsExchange asks one server over UDP, and again over TCP when the answer
// was cut short.
func dnsExchange(ctx context.Context, server string, q []byte, id uint16) ([]byte, error) {
	d := net.Dialer{Timeout: dnsTimeout}
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, err
	}
	// Only the answer matters, and a close error cannot change it.
	defer func() { _ = conn.Close() }()
	// If this fails, the conn is closed already, which wakes the read too.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(dnsTimeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 12 && binary.BigEndian.Uint16(buf[:2]) == id {
			if buf[2]&0x02 == 0 {
				return buf[:n], nil
			}
			break
		}
	}

	// Truncated: TCP, with a two-byte length before each message.
	tc, err := dnsDialTCP(ctx, &d, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tc.Close() }()
	stopTCP := context.AfterFunc(ctx, func() { _ = tc.SetDeadline(time.Now()) })
	defer stopTCP()
	if err := tc.SetDeadline(time.Now().Add(dnsTimeout)); err != nil {
		return nil, err
	}
	if _, err := tc.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		return nil, err
	}
	var lenb [2]byte
	if _, err := io.ReadFull(tc, lenb[:]); err != nil {
		return nil, err
	}
	answer := make([]byte, binary.BigEndian.Uint16(lenb[:]))
	if _, err := io.ReadFull(tc, answer); err != nil {
		return nil, err
	}
	if len(answer) < 12 || binary.BigEndian.Uint16(answer[:2]) != id {
		return nil, errors.New("hostnet: bad DNS answer")
	}
	return answer, nil
}

// dnsHostServers finds the host's nameservers. Tests replace it.
var dnsHostServers = dnsSystemServers

// dnsServers returns the host's nameservers, as host:port.
func dnsServers() []string {
	servers := dnsHostServers()
	if len(servers) == 0 {
		// The default of glibc and Go's resolver.
		servers = []string{"127.0.0.1:53", "[::1]:53"}
	}
	return servers
}
