//go:build windows

//declscope:namespace dns

package hostnet

import (
	"errors"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// dnsRawSockaddr lays out a sockaddr as GetAdaptersAddresses does: the
// family, then the port and the address in network order.
func dnsRawSockaddr(family uint16, b ...byte) *syscall.RawSockaddrAny {
	raw := &syscall.RawSockaddrAny{Addr: syscall.RawSockaddr{Family: family}}
	for i, c := range b {
		if i < len(raw.Addr.Data) {
			raw.Addr.Data[i] = int8(c)
		} else {
			raw.Pad[i-len(raw.Addr.Data)] = int8(c)
		}
	}
	return raw
}

// TestDNSAdapterServers lists the servers of adapters that are up. It
// skips what Windows lists when none is set, and addresses that are no IP.
func TestDNSAdapterServers(t *testing.T) {
	addr := func(raw *syscall.RawSockaddrAny, next *windows.IpAdapterDnsServerAdapter) *windows.IpAdapterDnsServerAdapter {
		return &windows.IpAdapterDnsServerAdapter{Address: windows.SocketAddress{Sockaddr: raw}, Next: next}
	}
	// Port 53 in network order, then 192.0.2.1.
	v4 := dnsRawSockaddr(syscall.AF_INET, 0, 53, 192, 0, 2, 1)
	// Port, flow info, then 2001:db8::1.
	v6 := dnsRawSockaddr(syscall.AF_INET6, 0, 53, 0, 0, 0, 0, 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
	siteLocal := dnsRawSockaddr(syscall.AF_INET6, 0, 53, 0, 0, 0, 0, 0xfe, 0xc0, 0, 0, 0, 0, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 1)
	unix := dnsRawSockaddr(syscall.AF_UNIX, 'x')
	unknown := dnsRawSockaddr(255)
	down := &windows.IpAdapterAddresses{OperStatus: windows.IfOperStatusDown, FirstDnsServerAddress: addr(v4, nil)}
	up := &windows.IpAdapterAddresses{
		OperStatus:            windows.IfOperStatusUp,
		FirstDnsServerAddress: addr(unknown, addr(unix, addr(siteLocal, addr(v6, addr(v4, nil))))),
		Next:                  down,
	}
	if got, want := dnsAdapterServers(up), []string{"[2001:db8::1]:53", "192.0.2.1:53"}; !slices.Equal(got, want) {
		t.Errorf("servers %q, want %q", got, want)
	}
}

// TestDNSSystemServersFails: no list, and so no servers, when
// GetAdaptersAddresses fails, or asks for no more room than it had.
func TestDNSSystemServersFails(t *testing.T) {
	get := dnsGetAdaptersAddresses
	t.Cleanup(func() { dnsGetAdaptersAddresses = get })
	for name, fake := range map[string]func(size *uint32) error{
		"failed":    func(*uint32) error { return windows.ERROR_NO_DATA },
		"same size": func(*uint32) error { return windows.ERROR_BUFFER_OVERFLOW },
	} {
		calls := 0
		dnsGetAdaptersAddresses = func(_, _ uint32, _ uintptr, _ *windows.IpAdapterAddresses, size *uint32) error {
			calls++
			return fake(size)
		}
		if got := dnsSystemServers(); got != nil || calls != 1 {
			t.Errorf("%s: %q after %d calls", name, got, calls)
		}
	}
	// More room first, then a failure.
	calls := 0
	dnsGetAdaptersAddresses = func(_, _ uint32, _ uintptr, _ *windows.IpAdapterAddresses, size *uint32) error {
		calls++
		if calls == 1 {
			*size *= 2
			return windows.ERROR_BUFFER_OVERFLOW
		}
		return errors.New("gone")
	}
	if got := dnsSystemServers(); got != nil || calls != 2 {
		t.Errorf("grown: %q after %d calls", got, calls)
	}
}
