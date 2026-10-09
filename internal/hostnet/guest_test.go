package hostnet

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// guestModuleWasm is a module with one page of exported memory and nothing
// else, for calling host functions directly.
var guestModuleWasm = []byte{
	0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01, // memory: one, min 1 page
	0x07, 0x07, 0x01, 0x03, 'm', 'e', 'm', 0x02, 0x00, // export "mem"
}

// newGuest returns a module whose memory the host functions read and write.
//
//declscope:shared // socket_test.go and dns_test.go call host functions with it
func newGuest(t *testing.T) api.Module {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	m, err := r.Instantiate(ctx, guestModuleWasm)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// guestString writes s at off and returns where it is.
//
//declscope:shared // socket_test.go and dns_test.go
func guestString(t *testing.T, m api.Module, off uint32, s string) (uint32, uint32) {
	t.Helper()
	if !m.Memory().WriteString(off, s) {
		t.Fatalf("cannot write %q at %d", s, off)
	}
	return off, uint32(len(s))
}

// guestRead returns n bytes at off as a string.
//
//declscope:shared // socket_test.go and dns_test.go
func guestRead(t *testing.T, m api.Module, off, n uint32) string {
	t.Helper()
	b, ok := m.Memory().Read(off, n)
	if !ok {
		t.Fatalf("cannot read %d bytes at %d", n, off)
	}
	return string(b)
}

// guestRun is a Run whose interrupts the test fires.
//
//declscope:shared // socket_test.go and dns_test.go
type guestRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	intr   chan struct{}
}

// newGuestRun returns a run that lasts until the test ends.
//
//declscope:shared // socket_test.go and dns_test.go
func newGuestRun(t *testing.T) *guestRun {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &guestRun{ctx: ctx, cancel: cancel, intr: make(chan struct{})}
}

func (r *guestRun) Context() context.Context      { return r.ctx }
func (r *guestRun) Interruption() <-chan struct{} { return r.intr }
