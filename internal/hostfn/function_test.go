package hostfn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// functionGuestWasm is a module with one page of exported memory.
var functionGuestWasm = []byte{
	0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x07, 0x01, 0x03, 'm', 'e', 'm', 0x02, 0x00,
}

const (
	functionNameOff = 0
	functionArgsOff = 1024
	functionOutOff  = 8192
)

type functionHarness struct {
	t *testing.T
	m api.Module
	f *Functions
	x functionExports
}

func newFunctionHarness(t *testing.T, fns map[string]Function) *functionHarness {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { _ = r.Close(ctx) })
	m, err := r.Instantiate(ctx, functionGuestWasm)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFunctions(ctx, fns)
	return &functionHarness{t: t, m: m, f: f, x: functionExports{from: func(context.Context) *Functions { return f }}}
}

// call calls name with raw arguments, and returns the call's result and
// what fn_take then copies.
func (h *functionHarness) call(name string, args []byte) (int32, []byte) {
	h.t.Helper()
	h.m.Memory().WriteString(functionNameOff, name)
	h.m.Memory().Write(functionArgsOff, args)
	r := h.x.call(context.Background(), h.m, functionNameOff, uint32(len(name)), functionArgsOff, uint32(len(args)))
	n := r
	if r < 0 {
		n = -1 - r
	}
	got := h.x.take(context.Background(), h.m, functionOutOff, n)
	b, _ := h.m.Memory().Read(functionOutOff, uint32(got))
	return r, append([]byte(nil), b...)
}

func (h *functionHarness) encode(v any) []byte {
	h.t.Helper()
	b, err := valueEncode(nil, v)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func TestFunctionNames(t *testing.T) {
	h := newFunctionHarness(t, map[string]Function{"Go_B": nil, "go_a": nil})
	n := h.x.names(context.Background(), h.m, functionOutOff, 2)
	if b, _ := h.m.Memory().Read(functionOutOff, 2); n != 10 || string(b) != "\x00\x00" {
		t.Errorf("names with no room = %d, wrote %q", n, b)
	}
	n = h.x.names(context.Background(), h.m, functionOutOff, 64)
	if b, _ := h.m.Memory().Read(functionOutOff, uint32(n)); string(b) != "go_a\x00go_b\x00" {
		t.Errorf("names = %q", b)
	}
}

func TestFunctionCall(t *testing.T) {
	var gotArgs []any
	h := newFunctionHarness(t, map[string]Function{
		"Echo": func(_ context.Context, args []any) (any, error) {
			gotArgs = args
			return args, nil
		},
		"fail": func(context.Context, []any) (any, error) { return nil, errors.New("from Go") },
		"bad":  func(context.Context, []any) (any, error) { return make(chan int), nil },
	})

	args := []any{int64(1), "two", map[string]any{"k": []any{nil, true}}}
	r, out := h.call("ECHO", h.encode(args))
	if r != int32(len(out)) || !reflect.DeepEqual(gotArgs, args) {
		t.Fatalf("call = %d with %#v", r, gotArgs)
	}
	if v, _, err := valueDecode(out); err != nil || !reflect.DeepEqual(v, args) {
		t.Errorf("result %#v, %v", v, err)
	}
	if h.f.pending != nil {
		t.Error("take left the result pending")
	}

	for _, c := range []struct {
		name string
		args []byte
		want string
	}{
		{"fail", h.encode([]any{}), "from Go"},
		{"bad", h.encode([]any{}), "cannot be a PHP value"},
		{"missing", h.encode([]any{}), "no such Go function: missing"},
		{"echo", h.encode([]any{})[:2], "cut short"},
		{"echo", append(h.encode([]any{}), h.encode(nil)...), "extra bytes"},
	} {
		r, out := h.call(c.name, c.args)
		if r >= 0 || int(-1-r) != len(out) || !strings.Contains(string(out), c.want) {
			t.Errorf("%s(% x) = %d, %q; want an error with %q", c.name, c.args, r, out, c.want)
		}
	}

	// fn_take copies no more than it is given room for.
	h.m.Memory().WriteString(functionNameOff, "fail")
	h.m.Memory().Write(functionArgsOff, h.encode([]any{}))
	h.x.call(context.Background(), h.m, functionNameOff, 4, functionArgsOff, 5)
	if n := h.x.take(context.Background(), h.m, functionOutOff, 4); n != 4 {
		t.Errorf("take = %d", n)
	}
	if b, _ := h.m.Memory().Read(functionOutOff, 4); string(b) != "from" {
		t.Errorf("took %q", b)
	}
	// A negative room is none.
	h.x.call(context.Background(), h.m, functionNameOff, 4, functionArgsOff, 5)
	if n := h.x.take(context.Background(), h.m, functionOutOff, -1); n != 0 {
		t.Errorf("take = %d", n)
	}
}
