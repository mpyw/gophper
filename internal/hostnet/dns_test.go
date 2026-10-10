package hostnet

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
)

const (
	dnsTestNameOff = 0
	dnsTestOutOff  = 1024
)

func newDNSExports(run wasi.Run) dnsExports {
	t := NewSockets(run, nil, true)
	return dnsExports{from: func(context.Context) *Sockets { return t }}
}

func TestDNSReverse(t *testing.T) {
	m := newGuest(t)
	x := newDNSExports(newGuestRun(t))
	reverse := func(addr string, capacity uint32) (string, int32) {
		p, n := guestString(t, m, dnsTestNameOff, addr)
		r := x.reverse(context.Background(), m, p, n, dnsTestOutOff, capacity)
		if r < 0 {
			return "", r
		}
		return guestRead(t, m, dnsTestOutOff, uint32(r)), r
	}

	// The host's answer, without the final dot.
	want := ""
	if names, err := net.DefaultResolver.LookupAddr(context.Background(), "127.0.0.1"); err == nil && len(names) > 0 {
		want = strings.TrimSuffix(names[0], ".")
	}
	got, r := reverse("127.0.0.1", 255)
	switch {
	case want == "" && r != -1:
		t.Errorf("reverse(127.0.0.1) = %q, %d; the host has no name for it", got, r)
	case want != "" && got != want:
		t.Errorf("reverse(127.0.0.1) = %q, want %q", got, want)
	}
	if want != "" {
		if got, _ := reverse("127.0.0.1", 3); got != want[:3] {
			t.Errorf("cut reverse = %q, want %q", got, want[:3])
		}
	}
	if got, r := reverse("not an address", 255); r != -1 {
		t.Errorf("reverse(not an address) = %q, %d", got, r)
	}
}

func TestDNSLookup(t *testing.T) {
	m := newGuest(t)
	run := newGuestRun(t)
	x := newDNSExports(run)
	lookup := func(name string, family int32, capacity uint32) (string, int32) {
		m.Memory().WriteString(dnsTestOutOff, strings.Repeat("\x00", 64))
		p, n := guestString(t, m, dnsTestNameOff, name)
		r := x.lookup(context.Background(), m, p, n, family, dnsTestOutOff, capacity)
		if r < 0 {
			return "", r
		}
		return strings.TrimRight(guestRead(t, m, dnsTestOutOff, 64), "\x00"), r
	}
	if got, r := lookup("127.0.0.1", 4, 255); got != "127.0.0.1" || r != 9 {
		t.Errorf("lookup(127.0.0.1, 4) = %q, %d", got, r)
	}
	if got, r := lookup("::1", 6, 255); got != "::1" || r != 3 {
		t.Errorf("lookup(::1, 6) = %q, %d", got, r)
	}
	if got, r := lookup("127.0.0.1", 0, 255); got != "127.0.0.1" {
		t.Errorf("lookup(127.0.0.1, 0) = %q, %d", got, r)
	}
	// Too little room: the length says how much, and nothing is written.
	if got, r := lookup("127.0.0.1", 4, 4); got != "" || r != 9 {
		t.Errorf("lookup with room for 4 = %q, %d", got, r)
	}
	if _, r := lookup("127.0.0.1", 6, 255); r >= 0 {
		t.Errorf("an IPv4 address as IPv6 = %d", r)
	}
}

func TestDNSQueryFails(t *testing.T) {
	m := newGuest(t)
	run := newGuestRun(t)
	x := newDNSExports(run)
	query := func(name string) int32 {
		p, n := guestString(t, m, dnsTestNameOff, name)
		return x.query(context.Background(), m, p, n, 1, 1, dnsTestOutOff, 512)
	}
	if r := query("bad..name"); r != -dnsNoRecovery {
		t.Errorf("query(bad..name) = %d, want %d", r, -dnsNoRecovery)
	}
	// Once the run is over, no server is asked.
	run.cancel()
	if r := query("example.com"); r != -dnsTryAgain {
		t.Errorf("query after the run = %d, want %d", r, -dnsTryAgain)
	}
}

func TestDNSBuildQuery(t *testing.T) {
	q, id, err := dnsBuildQuery("www.example.com.", 1, 15)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(q) != id || q[2] != 0x01 || binary.BigEndian.Uint16(q[4:]) != 1 {
		t.Errorf("header % x", q[:12])
	}
	if got, want := string(q[12:]), "\x03www\x07example\x03com\x00\x00\x0f\x00\x01"; got != want {
		t.Errorf("question %q, want %q", got, want)
	}
	for _, name := range []string{"a..b", strings.Repeat("x", 64) + ".com", ""} {
		if _, _, err := dnsBuildQuery(name, 1, 1); err == nil {
			t.Errorf("dnsBuildQuery(%q) succeeded", name)
		}
	}
}

func TestDNSServers(t *testing.T) {
	defer func(old func() []string) { dnsHostServers = old }(dnsHostServers)
	dnsHostServers = func() []string { return nil }
	if got := dnsServers(); !slices.Equal(got, []string{"127.0.0.1:53", "[::1]:53"}) {
		t.Errorf("without servers: %v", got)
	}
	dnsHostServers = dnsSystemServers
	servers := dnsServers()
	if len(servers) == 0 {
		t.Fatal("no servers")
	}
	for _, s := range servers {
		if _, port, err := net.SplitHostPort(s); err != nil || port != "53" {
			t.Errorf("server %q", s)
		}
	}
}

// dnsTestAnswer returns a reply to q with the given id and flags.
func dnsTestAnswer(q []byte, id uint16, flags byte) []byte {
	a := append([]byte(nil), q...)
	binary.BigEndian.PutUint16(a, id)
	a[2] = 0x80 | flags
	return a
}

// dnsTestServer listens on UDP and TCP on one local port. udp answers each
// query with any number of packets, and tcp answers over TCP.
func dnsTestServer(t *testing.T, udp func(q []byte) [][]byte, tcp func(q []byte) []byte) string {
	t.Helper()
	var pc net.PacketConn
	var l net.Listener
	for range 10 {
		var err error
		if pc, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if l, err = net.Listen("tcp", pc.LocalAddr().String()); err == nil {
			break
		}
		_ = pc.Close()
	}
	if l == nil {
		t.Skip("no port free for both UDP and TCP")
	}
	t.Cleanup(func() { _ = pc.Close(); _ = l.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			for _, p := range udp(append([]byte(nil), buf[:n]...)) {
				// A lost answer shows as a failed exchange in the test.
				_, _ = pc.WriteTo(p, from)
			}
		}
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			var lenb [2]byte
			if _, err := io.ReadFull(c, lenb[:]); err != nil {
				_ = c.Close()
				continue
			}
			q := make([]byte, binary.BigEndian.Uint16(lenb[:]))
			if _, err := io.ReadFull(c, q); err != nil {
				_ = c.Close()
				continue
			}
			if a := tcp(q); a != nil {
				// A lost answer shows as a failed exchange in the test.
				_, _ = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(a))), a...))
			}
			_ = c.Close()
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNSExchange(t *testing.T) {
	q, id, err := dnsBuildQuery("example.com", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	noTCP := func([]byte) []byte { t.Error("asked over TCP"); return nil }
	noUDP := func([]byte) [][]byte { t.Error("asked over UDP"); return nil }

	// Packets that are too short or for another query are skipped.
	server := dnsTestServer(t, func(q []byte) [][]byte {
		return [][]byte{{1, 2, 3}, dnsTestAnswer(q, id+1, 0), dnsTestAnswer(q, id, 0)}
	}, noTCP)
	a, err := dnsExchange(ctx, server, q, id)
	if err != nil || binary.BigEndian.Uint16(a) != id || a[2]&0x02 != 0 {
		t.Errorf("UDP answer % x, %v", a, err)
	}

	// A truncated answer is asked again over TCP.
	truncated := func(q []byte) [][]byte { return [][]byte{dnsTestAnswer(q, id, 0x02)} }
	server = dnsTestServer(t, truncated, func(q []byte) []byte {
		return append(dnsTestAnswer(q, id, 0), "full"...)
	})
	if a, err := dnsExchange(ctx, server, q, id); err != nil || !strings.HasSuffix(string(a), "full") {
		t.Errorf("TCP answer %q, %v", a, err)
	}

	for name, tcp := range map[string]func([]byte) []byte{
		"another id": func(q []byte) []byte { return dnsTestAnswer(q, id+1, 0) },
		"too short":  func([]byte) []byte { return []byte{1, 2} },
		"no answer":  func([]byte) []byte { return nil },
	} {
		server = dnsTestServer(t, truncated, tcp)
		if a, err := dnsExchange(ctx, server, q, id); err == nil {
			t.Errorf("%s over TCP: % x", name, a)
		}
	}

	// A query too long for one datagram is refused when it is sent.
	if a, err := dnsExchange(ctx, dnsTestUDPOnly(t, noUDP), make([]byte, 70000), id); err == nil {
		t.Errorf("oversized query: % x", a)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dnsExchange(canceled, server, q, id); err == nil {
		t.Error("exchange with a canceled context succeeded")
	}

	// Canceling cuts a wait for either answer short.
	silent := func([]byte) [][]byte { return nil }
	slowTCP := func([]byte) []byte { time.Sleep(time.Second); return nil }
	for name, server := range map[string]string{
		"UDP": dnsTestServer(t, silent, noTCP),
		"TCP": dnsTestServer(t, truncated, slowTCP),
	} {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		start := time.Now()
		_, err := dnsExchange(ctx, server, q, id)
		cancel()
		if err == nil || time.Since(start) > dnsTimeout/2 {
			t.Errorf("canceled while waiting over %s: %v after %s", name, err, time.Since(start))
		}
	}

	// A truncated answer from a server with no TCP, or with an answer
	// shorter than its length.
	for name, tcp := range map[string]func(net.Conn){
		"no TCP":       nil,
		"short answer": func(c net.Conn) { _, _ = c.Write([]byte{0, 100, 1, 2}) },
	} {
		server := dnsTestUDPOnly(t, truncated)
		if tcp != nil {
			l, err := net.Listen("tcp", server)
			if err != nil {
				t.Skip(err)
			}
			defer func() { _ = l.Close() }()
			go func() {
				c, err := l.Accept()
				if err != nil {
					return
				}
				tcp(c)
				_ = c.Close()
			}()
		}
		if a, err := dnsExchange(ctx, server, q, id); err == nil {
			t.Errorf("%s: % x", name, a)
		}
	}
}

// dnsTestUDPOnly listens on UDP alone. udp answers each query.
func dnsTestUDPOnly(t *testing.T, udp func(q []byte) [][]byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			for _, p := range udp(append([]byte(nil), buf[:n]...)) {
				_, _ = pc.WriteTo(p, from) // a lost answer fails the exchange
			}
		}
	}()
	return pc.LocalAddr().String()
}

// TestDNSQueryAnswers maps the answer's rcode and count to h_errno, and
// tries the next server when one fails.
func TestDNSQueryAnswers(t *testing.T) {
	defer func(old func() []string) { dnsHostServers = old }(dnsHostServers)
	m := newGuest(t)
	x := newDNSExports(newGuestRun(t))
	answer := func(rcode byte, count uint16) func(q []byte) [][]byte {
		return func(q []byte) [][]byte {
			a := dnsTestAnswer(q, binary.BigEndian.Uint16(q), 0)
			a[3] = rcode
			binary.BigEndian.PutUint16(a[6:], count)
			return [][]byte{a}
		}
	}
	noTCP := func([]byte) []byte { return nil }
	// 256.0.0.1 is no address, so dialing it fails at once.
	for _, c := range []struct {
		name    string
		servers []string
		want    int32
	}{
		{"answer", []string{dnsTestServer(t, answer(0, 1), noTCP)}, 0},
		{"no data", []string{dnsTestServer(t, answer(0, 0), noTCP)}, -dnsNoData},
		{"NXDOMAIN", []string{dnsTestServer(t, answer(3, 0), noTCP)}, -dnsHostNotFound},
		{"SERVFAIL", []string{dnsTestServer(t, answer(2, 0), noTCP)}, -dnsTryAgain},
		{"REFUSED", []string{dnsTestServer(t, answer(5, 0), noTCP)}, -dnsNoRecovery},
		{"no server answers", []string{"256.0.0.1:53"}, -dnsTryAgain},
		{"the next server", []string{"256.0.0.1:53", dnsTestServer(t, answer(0, 1), noTCP)}, 0},
	} {
		dnsHostServers = func() []string { return c.servers }
		p, n := guestString(t, m, dnsTestNameOff, "example.com")
		got := x.query(context.Background(), m, p, n, 1, 1, dnsTestOutOff, 512)
		// 0 stands for an answer, of more than a header.
		if (c.want == 0 && got <= 12) || (c.want != 0 && got != c.want) {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// TestDNSNoNetwork answers every lookup as failed, with no query sent.
func TestDNSNoNetwork(t *testing.T) {
	m := newGuest(t)
	tab := NewSockets(newGuestRun(t), nil, false)
	x := dnsExports{from: func(context.Context) *Sockets { return tab }}
	p, n := guestString(t, m, dnsTestNameOff, "example.com")
	if r := x.lookup(context.Background(), m, p, n, 0, dnsTestOutOff, 512); r != -2 {
		t.Errorf("lookup = %d, want -2 (EAI_FAIL)", r)
	}
	p, n = guestString(t, m, dnsTestNameOff, "127.0.0.1")
	if r := x.reverse(context.Background(), m, p, n, dnsTestOutOff, 512); r != -1 {
		t.Errorf("reverse = %d, want -1", r)
	}
	p, n = guestString(t, m, dnsTestNameOff, "example.com")
	if r := x.query(context.Background(), m, p, n, 1, 1, dnsTestOutOff, 512); r != -dnsNoRecovery {
		t.Errorf("query = %d, want %d", r, -dnsNoRecovery)
	}
	// Outside any instance, as from returns nil.
	none := dnsExports{from: func(context.Context) *Sockets { return nil }}
	if r := none.lookup(context.Background(), m, p, n, 0, dnsTestOutOff, 512); r != -2 {
		t.Errorf("no instance: lookup = %d", r)
	}
}
