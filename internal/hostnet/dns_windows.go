//go:build windows

//declscope:namespace dns

package hostnet

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"unsafe" //nolint:depguard // GetAdaptersAddresses fills a raw buffer of linked structs.

	"golang.org/x/sys/windows"
)

// dnsSystemServers lists the nameservers of every adapter that is up, as
// Go's resolver does on Windows (net/dnsconfig_windows.go).
func dnsSystemServers() []string {
	size := uint32(15000)
	var buf []byte
	for {
		buf = make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(syscall.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_PREFIX, 0, first, &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) || size <= uint32(len(buf)) {
			return nil
		}
	}
	var servers []string
	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		if aa.OperStatus != windows.IfOperStatusUp {
			continue
		}
		for dns := aa.FirstDnsServerAddress; dns != nil; dns = dns.Next {
			sa, err := dns.Address.Sockaddr.Sockaddr()
			if err != nil {
				continue
			}
			var ip netip.Addr
			switch sa := sa.(type) {
			case *syscall.SockaddrInet4:
				ip = netip.AddrFrom4(sa.Addr)
			case *syscall.SockaddrInet6:
				ip = netip.AddrFrom16(sa.Addr)
				// fec0:0:0:ffff::1 to ::3 are deprecated site-local defaults
				// that Windows lists when no server is set.
				if b := ip.As16(); b[0] == 0xfe && b[1] == 0xc0 && b[2] == 0 && b[3] == 0 && b[6] == 0xff && b[7] == 0xff {
					continue
				}
			default:
				continue
			}
			servers = append(servers, net.JoinHostPort(ip.String(), "53"))
		}
	}
	return servers
}
