package hostnet

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ExportDNS adds the resolver host functions, used by getaddrinfo(3) and
// friends in gophper-wasm's compat/gophper_net.c. from returns the instance a call
// comes from, so a lookup stops at its interrupts.
//
// The lookups fail, with no query sent, for an instance whose Sockets has
// no network.
func ExportDNS(b wazero.HostModuleBuilder, from func(context.Context) *Sockets) {
	x := dnsExports{from: from}
	b.NewFunctionBuilder().WithFunc(x.lookup).Export("dns_lookup")
	b.NewFunctionBuilder().WithFunc(x.reverse).Export("dns_reverse")
	b.NewFunctionBuilder().WithFunc(x.query).Export("dns_query")
}

type dnsExports struct {
	from func(context.Context) *Sockets
}

// run returns the run a call comes from, and whether it may use DNS.
func (x dnsExports) run(ctx context.Context) (wasi.Run, bool) {
	t := x.from(ctx)
	if t == nil {
		return nil, false
	}
	return t.Run(), t.Network()
}

// lookup writes name's addresses as lines of text. family is 4, 6 or 0
// for both. It returns the length, which exceeds outCap when the caller
// must retry with more room, or -1 for an unknown name and -2 otherwise.
func (x dnsExports) lookup(ctx context.Context, m api.Module, namePtr, nameLen uint32, family int32, outPtr, outCap uint32) int32 {
	name, _ := m.Memory().Read(namePtr, nameLen)
	network := "ip"
	switch family {
	case 4:
		network = "ip4"
	case 6:
		network = "ip6"
	}
	run, ok := x.run(ctx)
	if !ok {
		return -2 // EAI_FAIL: no use asking again
	}
	lctx, cancel := interruptibleRunContext(run)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(lctx, network, string(name))
	if err != nil {
		if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
			return -1
		}
		return -2
	}
	lines := make([]string, len(ips))
	for i, ip := range ips {
		lines[i] = ip.String()
	}
	text := strings.Join(lines, "\n")
	if len(text) <= int(outCap) {
		m.Memory().WriteString(outPtr, text)
	}
	return int32(len(text))
}

// reverse writes the first name for an address, or returns -1.
func (x dnsExports) reverse(ctx context.Context, m api.Module, addrPtr, addrLen, outPtr, outCap uint32) int32 {
	addr, _ := m.Memory().Read(addrPtr, addrLen)
	run, ok := x.run(ctx)
	if !ok {
		return -1
	}
	lctx, cancel := interruptibleRunContext(run)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(lctx, string(addr))
	if err != nil || len(names) == 0 {
		return -1
	}
	name := strings.TrimSuffix(names[0], ".")
	if len(name) > int(outCap) {
		name = name[:outCap]
	}
	m.Memory().WriteString(outPtr, name)
	return int32(len(name))
}
