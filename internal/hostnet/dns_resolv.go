//go:build !windows

//declscope:namespace dns

package hostnet

import (
	"bufio"
	"net"
	"os"
	"strings"
)

// dnsResolvConf is where dnsSystemServers looks. Tests replace it.
var dnsResolvConf = "/etc/resolv.conf"

// dnsSystemServers reads the nameservers of /etc/resolv.conf.
func dnsSystemServers() []string {
	f, err := os.Open(dnsResolvConf)
	if err != nil {
		return nil
	}
	// Opened read-only, so closing it loses nothing.
	defer func() { _ = f.Close() }()
	var servers []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, net.JoinHostPort(strings.SplitN(fields[1], "%", 2)[0], "53"))
		}
	}
	return servers
}
