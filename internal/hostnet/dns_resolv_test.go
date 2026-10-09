//go:build !windows

//declscope:namespace dns

package hostnet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDNSSystemServers(t *testing.T) {
	defer func(old string) { dnsResolvConf = old }(dnsResolvConf)
	dnsResolvConf = filepath.Join(t.TempDir(), "resolv.conf")
	if got := dnsSystemServers(); got != nil {
		t.Errorf("no file: %v", got)
	}
	conf := "# comment\nsearch example.com\nnameserver 192.0.2.1\nnameserver fe80::1%en0\nnameserver\n"
	if err := os.WriteFile(dnsResolvConf, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := dnsSystemServers(), []string{"192.0.2.1:53", "[fe80::1]:53"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
