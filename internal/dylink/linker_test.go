package dylink

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// The modules below are written out byte by byte, as no assembler is at
// hand. linkerMainModule stands for php.wasm, and linkerSideModule for an
// extension built with -shared -fPIC.

func linkerULEB(v uint32) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			return out
		}
	}
}

func linkerName(s string) []byte { return append(linkerULEB(uint32(len(s))), s...) }

// linkerVec encodes a vector: its length, then its items.
func linkerVec(items ...[]byte) []byte {
	return slices.Concat(append([][]byte{linkerULEB(uint32(len(items)))}, items...)...)
}

func linkerSection(id byte, body []byte) []byte {
	return slices.Concat([]byte{id}, linkerULEB(uint32(len(body))), body)
}

// linkerCode encodes a function body without locals.
func linkerCode(instrs ...byte) []byte {
	body := append([]byte{0x00}, instrs...)
	return append(linkerULEB(uint32(len(body))), body...)
}

func linkerExport(name string, kind byte, idx uint32) []byte {
	return slices.Concat(linkerName(name), []byte{kind}, linkerULEB(idx))
}

func linkerImport(module, name string, kind byte, desc ...byte) []byte {
	return slices.Concat(linkerName(module), linkerName(name), []byte{kind}, desc)
}

const linkerWasmHeader = "\x00asm\x01\x00\x00\x00"

// linkerMainModule exports what the linker needs from php.wasm: calloc,
// which always returns 1024, the memory, the function table, and a
// function and a data symbol for side modules. invoke calls the table
// entry at its argument, as a C function pointer call does. tableMax,
// unless 0, caps the table.
func linkerMainModule(tableMax uint32) []byte {
	types := linkerVec(
		[]byte{0x60, 2, api.ValueTypeI32, api.ValueTypeI32, 1, api.ValueTypeI32}, // 0: calloc
		[]byte{0x60, 0, 1, api.ValueTypeI32},                                     // 1: () -> i32
		[]byte{0x60, 1, api.ValueTypeI32, 1, api.ValueTypeI32},                   // 2: invoke
	)
	table := []byte{0x70, 0x00, 0x01}
	if tableMax > 0 {
		table = slices.Concat([]byte{0x70, 0x01, 0x01}, linkerULEB(tableMax))
	}
	return slices.Concat([]byte(linkerWasmHeader),
		linkerSection(1, types),
		linkerSection(3, linkerVec([]byte{0}, []byte{2}, []byte{1})),
		linkerSection(4, linkerVec(table)),
		linkerSection(5, linkerVec([]byte{0x00, 0x01})),
		// main_data is at address 16.
		linkerSection(6, linkerVec([]byte{api.ValueTypeI32, 0x00, 0x41, 16, 0x0b})),
		linkerSection(7, linkerVec(
			linkerExport("calloc", 0, 0),
			linkerExport("invoke", 0, 1),
			linkerExport("main_func", 0, 2),
			linkerExport("memory", 2, 0),
			linkerExport("__indirect_function_table", 1, 0),
			linkerExport("main_data", 3, 0),
		)),
		linkerSection(10, linkerVec(
			linkerCode(0x41, 0x80, 0x08, 0x0b),       // i32.const 1024
			linkerCode(0x20, 0x00, 0x11, 1, 0, 0x0b), // call_indirect (type 1) (local.get 0)
			linkerCode(0x41, 7, 0x0b),                // i32.const 7
		)),
	)
}

// linkerSideModule is a side module of 16 bytes of data, aligned to 4. It
// uses main's function and data through env and the GOT, and its own
// function and data through the GOT too. extraImports come after its own.
func linkerSideModule(dylink bool, extraImports ...[]byte) []byte {
	types := linkerVec(
		[]byte{0x60, 0, 1, api.ValueTypeI32}, // 0: () -> i32
		[]byte{0x60, 0, 0},                   // 1: () -> ()
	)
	imports := linkerVec(append([][]byte{
		linkerImport("env", "main_func", 0, 0),                        // func 0
		linkerImport("env", "memory", 2, 0x00, 0x01),                  //
		linkerImport("env", "__memory_base", 3, api.ValueTypeI32, 0),  // global 0
		linkerImport("GOT.mem", "main_data", 3, api.ValueTypeI32, 1),  // global 1
		linkerImport("GOT.func", "main_func", 3, api.ValueTypeI32, 1), // global 2
		linkerImport("GOT.mem", "side_data", 3, api.ValueTypeI32, 1),  // global 3
		linkerImport("GOT.func", "side_func", 3, api.ValueTypeI32, 1), // global 4
	}, extraImports...)...)
	var funcs, globals int
	for _, imp := range extraImports {
		// The kind follows the two names.
		r := imp[1+int(imp[0]):]
		switch r[1+int(r[0])] {
		case 0:
			funcs++
		case 3:
			globals++
		}
	}
	fn := func(i int) uint32 { return uint32(1 + funcs + i) }
	sideData := uint32(5 + globals)
	var bin []byte
	bin = append(bin, linkerWasmHeader...)
	if dylink {
		memInfo := slices.Concat(linkerULEB(16), linkerULEB(2), linkerULEB(0), linkerULEB(0))
		bin = append(bin, linkerSection(0, slices.Concat(linkerName("dylink.0"), []byte{1}, linkerULEB(uint32(len(memInfo))), memInfo))...)
	}
	return slices.Concat(bin,
		linkerSection(1, types),
		linkerSection(2, imports),
		linkerSection(3, linkerVec([]byte{0}, []byte{0}, []byte{0}, []byte{0}, []byte{0}, []byte{1})),
		// side_data is at offset 8 from __memory_base.
		linkerSection(6, linkerVec([]byte{api.ValueTypeI32, 0x00, 0x41, 8, 0x0b})),
		linkerSection(7, linkerVec(
			linkerExport("side_func", 0, fn(0)),
			linkerExport("got_main_data", 0, fn(1)),
			linkerExport("got_main_func", 0, fn(2)),
			linkerExport("got_side_data", 0, fn(3)),
			linkerExport("got_side_func", 0, fn(4)),
			linkerExport("_initialize", 0, fn(5)),
			linkerExport("side_data", 3, sideData),
		)),
		linkerSection(10, linkerVec(
			linkerCode(0x41, 42, 0x0b), // i32.const 42
			linkerCode(0x23, 1, 0x0b),  // global.get
			linkerCode(0x23, 2, 0x0b),
			linkerCode(0x23, 3, 0x0b),
			linkerCode(0x23, 4, 0x0b),
			// *(int *)__memory_base = 99
			linkerCode(0x23, 0, 0x41, 0xe3, 0x00, 0x36, 0x02, 0x00, 0x0b),
		)),
	)
}

// linkerSetup instantiates a main module and a linker for it.
func linkerSetup(t *testing.T, tableMax uint32) (context.Context, api.Module, *Linker) {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesExceptionHandling|experimental.CoreFeaturesExtendedConst))
	t.Cleanup(func() { r.Close(ctx) })
	bin := linkerMainModule(tableMax)
	main, err := r.InstantiateWithConfig(ctx, bin, wazero.NewModuleConfig().WithName("main"))
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCache(r)
	if err := cache.AddMain(bin); err != nil {
		t.Fatal(err)
	}
	l := NewLinker(cache)
	t.Cleanup(func() { l.Close(ctx) })
	return ctx, main, l
}

func linkerCall(t *testing.T, ctx context.Context, m api.Module, name string, args ...uint64) uint32 {
	t.Helper()
	res, err := m.ExportedFunction(name).Call(ctx, args...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return uint32(res[0])
}

func TestLinkerOpenAndSym(t *testing.T) {
	t.Run("plain", func(t *testing.T) { linkerCheckOpenAndSym(t, linkerSideModule(true), false) })
	// More of what env provides: main's globals and table, __table_base,
	// and a weak function the side module defines and imports.
	t.Run("env", func(t *testing.T) {
		linkerCheckOpenAndSym(t, linkerSideModule(true,
			linkerImport("env", "__table_base", 3, api.ValueTypeI32, 0),
			linkerImport("env", "__indirect_function_table", 1, 0x70, 0x00, 0x00),
			linkerImport("env", "main_data", 3, api.ValueTypeI32, 0),
			linkerImport("env", "side_func", 0, 0),
		), true)
	})
}

// linkerCheckOpenAndSym links bin, which imports side_func from env when
// weak is set.
func linkerCheckOpenAndSym(t *testing.T, bin []byte, weak bool) {
	ctx, main, l := linkerSetup(t, 0)
	h, err := l.Open(ctx, main, bin)
	if err != nil {
		t.Fatal(err)
	}
	side := l.libs[h].mod
	const memBase = 1024

	// _initialize ran, with __memory_base where calloc put the data.
	if v, _ := main.Memory().ReadUint32Le(memBase); v != 99 {
		t.Errorf("_initialize wrote %d at __memory_base", v)
	}
	// A weak import goes through env's trampoline into the module's own function.
	var trampolines int
	for _, m := range l.modules {
		if f := m.ExportedFunction("side_func"); f != nil && m != side {
			trampolines++
			if got := linkerCall(t, ctx, m, "side_func"); got != 42 {
				t.Errorf("the trampoline calls into %d, want 42", got)
			}
		}
	}
	if want := map[bool]int{false: 0, true: 1}[weak]; trampolines != want {
		t.Errorf("%d trampolines, want %d", trampolines, want)
	}
	// The GOT holds main's data address, and the side module's own.
	if got := linkerCall(t, ctx, side, "got_main_data"); got != 16 {
		t.Errorf("GOT.mem main_data = %d, want 16", got)
	}
	if got := linkerCall(t, ctx, side, "got_side_data"); got != memBase+8 {
		t.Errorf("GOT.mem side_data = %d, want %d", got, memBase+8)
	}
	// The GOT's function pointers call the functions.
	if got := linkerCall(t, ctx, main, "invoke", uint64(linkerCall(t, ctx, side, "got_main_func"))); got != 7 {
		t.Errorf("GOT.func main_func calls into %d, want 7", got)
	}
	if got := linkerCall(t, ctx, main, "invoke", uint64(linkerCall(t, ctx, side, "got_side_func"))); got != 42 {
		t.Errorf("GOT.func side_func calls into %d, want 42", got)
	}

	// dlsym: a function is a table index, the same one each time.
	slot, err := l.Sym(ctx, main, h, "side_func")
	if err != nil {
		t.Fatal(err)
	}
	if got := linkerCall(t, ctx, main, "invoke", uint64(slot)); got != 42 {
		t.Errorf("Sym(side_func) calls into %d, want 42", got)
	}
	if again, err := l.Sym(ctx, main, h, "side_func"); err != nil || again != slot {
		t.Errorf("Sym(side_func) again = %d, %v, want %d", again, err, slot)
	}
	// Data is an address.
	if addr, err := l.Sym(ctx, main, h, "side_data"); err != nil || addr != memBase+8 {
		t.Errorf("Sym(side_data) = %d, %v, want %d", addr, err, memBase+8)
	}
	if _, err := l.Sym(ctx, main, h, "missing"); err == nil || err.Error() != "undefined symbol: missing" {
		t.Errorf("Sym(missing) = %v", err)
	}
	if _, err := l.Sym(ctx, main, h+1, "side_func"); err == nil || !strings.Contains(err.Error(), "invalid handle") {
		t.Errorf("Sym on a bad handle = %v", err)
	}
}

func TestLinkerOpenErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  []byte
		want string
	}{
		{"no dylink.0", linkerSideModule(false), "not a shared library: no dylink.0 section"},
		{"not wasm", []byte("not a module"), "not a wasm module"},
		{"undefined global", linkerSideModule(true, linkerImport("env", "nope", 3, api.ValueTypeI32, 0)), "undefined symbol: nope"},
		{"undefined data", linkerSideModule(true, linkerImport("GOT.mem", "nope", 3, api.ValueTypeI32, 1)), "undefined symbol: nope"},
		{"undefined tag", linkerSideModule(true, linkerImport("env", "nope", 4, 0x00, 1)), "undefined tag: nope"},
		{"unknown import module", linkerSideModule(true, linkerImport("wasi", "f", 0, 1)), `unsupported import module "wasi"`},
		// main lacks it, and the side module does not export it.
		{"undefined function", linkerSideModule(true, linkerImport("env", "nope", 0, 1)), "undefined symbol: nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, main, l := linkerSetup(t, 0)
			if _, err := l.Open(ctx, main, tc.bin); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Open = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLinkerTableFull(t *testing.T) {
	ctx, main, l := linkerSetup(t, 1)
	if _, err := l.Open(ctx, main, linkerSideModule(true)); err == nil || !strings.Contains(err.Error(), "cannot grow the function table") {
		t.Errorf("Open = %v", err)
	}
}

// The host functions behind dlopen, dlsym and dlerror.
func TestLinkerHostFuncs(t *testing.T) {
	ctx, main, l := linkerSetup(t, 0)
	x := linkerHostFuncs{from: func(context.Context) *Linker { return l }}
	mem := main.Memory()
	dlerror := func(capacity uint32) string {
		n := x.error(ctx, main, 4096, capacity)
		b, _ := mem.Read(4096, uint32(n))
		return string(b)
	}

	if h := x.open(ctx, main, mem.Size(), 8); h != 0 || dlerror(100) != "dlopen: out of bounds" {
		t.Errorf("dlopen out of bounds = %d, %q", h, dlerror(100))
	}
	if h := x.open(ctx, main, 8192, 4); h != 0 || dlerror(100) != "not a wasm module" {
		t.Errorf("dlopen of zeros = %d, %q", h, dlerror(100))
	}
	// dlerror truncates to the buffer.
	if got := dlerror(3); got != "not" {
		t.Errorf("dlerror(3) = %q", got)
	}

	side := linkerSideModule(true)
	mem.Write(8192, side)
	h := x.open(ctx, main, 8192, uint32(len(side)))
	if h == 0 {
		t.Fatalf("dlopen: %s", dlerror(200))
	}
	mem.WriteString(2048, "side_func")
	if ok := x.sym(ctx, main, h, 2048, 9, 2100); ok != 1 {
		t.Fatalf("dlsym: %s", dlerror(200))
	}
	slot, _ := mem.ReadUint32Le(2100)
	if got := linkerCall(t, ctx, main, "invoke", uint64(slot)); got != 42 {
		t.Errorf("dlsym(side_func) calls into %d", got)
	}
	if ok := x.sym(ctx, main, h, 2048, 4, 2100); ok != 0 || dlerror(200) != "undefined symbol: side" {
		t.Errorf("dlsym(side) = %d, %q", ok, dlerror(200))
	}
	if x.close(ctx, h) != 1 {
		t.Error("dlclose failed")
	}
}
