package hostsys

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// newGuest returns a module with one page of linear memory, to call
// the host functions with as the guest would.
//
//declscope:shared // user_test.go, path_test.go and lock_test.go
func newGuest(t *testing.T) api.Module {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	// A module that defines only a memory of one page.
	m, err := r.Instantiate(ctx, []byte("\x00asm\x01\x00\x00\x00\x05\x03\x01\x00\x01"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// guestRun is a wasi.Run whose interrupt and context the test controls.
//
//declscope:shared // path_test.go and lock_test.go
type guestRun struct {
	ctx  context.Context
	intr chan struct{}
}

func (r guestRun) Context() context.Context      { return r.ctx }
func (r guestRun) Interruption() <-chan struct{} { return r.intr }
