// Package hostfn runs PHP functions written in Go. gophper-wasm's
// compat/gophper_fn.c registers each name as an internal function at
// startup, and its calls come here.
package hostfn

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Function is a PHP function written in Go. See gophper.Function.
type Function func(ctx context.Context, args []any) (any, error)

// Functions holds the functions of one PHP instance, and the result of the
// last call until the guest takes it.
type Functions struct {
	ctx     context.Context
	fns     map[string]Function
	names   []string
	pending []byte
}

// NewFunctions returns the functions for one PHP instance. PHP names are
// case-insensitive, so they are kept in lower case.
func NewFunctions(ctx context.Context, fns map[string]Function) *Functions {
	f := &Functions{ctx: ctx, fns: make(map[string]Function, len(fns))}
	for name, fn := range fns {
		lower := strings.ToLower(name)
		f.fns[lower] = fn
		f.names = append(f.names, lower)
	}
	slices.Sort(f.names)
	return f
}

// ExportFunctions adds the host functions to b, the "gophper" host module.
func ExportFunctions(b wazero.HostModuleBuilder, from func(context.Context) *Functions) {
	x := functionExports{from: from}
	b.NewFunctionBuilder().WithFunc(x.names).Export("fn_names")
	b.NewFunctionBuilder().WithFunc(x.call).Export("fn_call")
	b.NewFunctionBuilder().WithFunc(x.take).Export("fn_take")
}

type functionExports struct {
	from func(context.Context) *Functions
}

// names writes the function names, each ending with a NUL, and returns
// the length. More than capacity asks for a retry with that room.
func (x functionExports) names(ctx context.Context, m api.Module, out uint32, capacity int32) int32 {
	f := x.from(ctx)
	var b []byte
	for _, n := range f.names {
		b = append(append(b, n...), 0)
	}
	if len(b) <= int(capacity) {
		m.Memory().Write(out, b)
	}
	return int32(len(b))
}

// call runs a function with encoded arguments. It returns the length of
// the encoded result, or for an error, minus one minus the length of its
// message. fn_take then copies either.
func (x functionExports) call(ctx context.Context, m api.Module, namePtr, nameLen, argsPtr, argsLen uint32) int32 {
	f := x.from(ctx)
	name, _ := m.Memory().Read(namePtr, nameLen)
	raw, _ := m.Memory().Read(argsPtr, argsLen)
	fail := func(err error) int32 {
		f.pending = []byte(err.Error())
		return -1 - int32(len(f.pending))
	}
	fn := f.fns[strings.ToLower(string(name))]
	if fn == nil {
		return fail(errors.New("no such Go function: " + string(name)))
	}
	decoded, rest, err := valueDecode(raw)
	if err != nil {
		return fail(err)
	}
	if len(rest) > 0 {
		return fail(errors.New("hostfn: extra bytes after the arguments"))
	}
	args, _ := decoded.([]any)
	ret, err := fn(f.ctx, args)
	if err != nil {
		return fail(err)
	}
	if f.pending, err = valueEncode(nil, ret); err != nil {
		return fail(err)
	}
	return int32(len(f.pending))
}

// take copies the pending result or message, and forgets it.
func (x functionExports) take(ctx context.Context, m api.Module, out uint32, capacity int32) int32 {
	f := x.from(ctx)
	n := max(min(len(f.pending), int(capacity)), 0)
	m.Memory().Write(out, f.pending[:n])
	f.pending = nil
	return int32(n)
}
