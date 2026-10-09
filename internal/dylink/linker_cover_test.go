//declscope:namespace linker

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

// The tests here reach the failures of Open and its helpers. Each needs a
// main module that lacks something php.wasm always has, or a runtime that
// cannot compile what the linker generates.

// linkerFeatures are what php.wasm needs, as gophper's Engine enables them.
const linkerFeatures = api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling | experimental.CoreFeaturesExtendedConst

// linkerMainVariant changes linkerMainModule's exports.
type linkerMainVariant struct {
	noCalloc   bool
	nullCalloc bool // calloc returns NULL
	noMemory   bool
	noTable    bool
	// wrapped adds __wrap_main_func, which returns 8, as gophper-wasm's
	// --wrap links do.
	wrapped bool
	// tag adds main_tag, a tag of no parameters, as setjmp's __c_longjmp.
	tag bool
}

// linkerVariantMain is linkerMainModule, changed by v.
func linkerVariantMain(v linkerMainVariant) []byte {
	types := linkerVec(
		[]byte{0x60, 2, api.ValueTypeI32, api.ValueTypeI32, 1, api.ValueTypeI32}, // 0: calloc
		[]byte{0x60, 0, 1, api.ValueTypeI32},                                     // 1: () -> i32
		[]byte{0x60, 1, api.ValueTypeI32, 1, api.ValueTypeI32},                   // 2: invoke
		[]byte{0x60, 0, 0}, // 3: () -> ()
	)
	callocBody := linkerCode(0x41, 0x80, 0x08, 0x0b) // i32.const 1024
	if v.nullCalloc {
		callocBody = linkerCode(0x41, 0, 0x0b)
	}
	var exports [][]byte
	if !v.noCalloc {
		exports = append(exports, linkerExport("calloc", 0, 0))
	}
	exports = append(exports, linkerExport("invoke", 0, 1), linkerExport("main_func", 0, 2), linkerExport("main_data", 3, 0))
	if v.wrapped {
		exports = append(exports, linkerExport("__wrap_main_func", 0, 3))
	}
	if !v.noMemory {
		exports = append(exports, linkerExport("memory", 2, 0))
	}
	if !v.noTable {
		exports = append(exports, linkerExport("__indirect_function_table", 1, 0))
	}
	var tags []byte
	if v.tag {
		// The tag section sits between memory and global.
		tags = linkerSection(13, linkerVec([]byte{0x00, 3}))
		exports = append(exports, linkerExport("main_tag", 4, 0))
	}
	return slices.Concat([]byte(linkerWasmHeader),
		linkerSection(1, types),
		linkerSection(3, linkerVec([]byte{0}, []byte{2}, []byte{1}, []byte{1})),
		linkerSection(4, linkerVec([]byte{0x70, 0x00, 0x01})),
		linkerSection(5, linkerVec([]byte{0x00, 0x01})),
		tags,
		linkerSection(6, linkerVec([]byte{api.ValueTypeI32, 0x00, 0x41, 16, 0x0b})),
		linkerSection(7, linkerVec(exports...)),
		linkerSection(10, linkerVec(
			callocBody,
			linkerCode(0x20, 0x00, 0x11, 1, 0, 0x0b), // call_indirect (type 1) (local.get 0)
			linkerCode(0x41, 7, 0x0b),                // i32.const 7
			linkerCode(0x41, 8, 0x0b),                // i32.const 8
		)),
	)
}

// linkerBareSide is a side module with no imports: 16 bytes of data and
// tableSize table slots. It exports only _initialize, whose body is init.
func linkerBareSide(tableSize uint32, init ...byte) []byte {
	memInfo := slices.Concat(linkerULEB(16), linkerULEB(2), linkerULEB(tableSize), linkerULEB(0))
	return slices.Concat([]byte(linkerWasmHeader),
		linkerSection(0, slices.Concat(linkerName("dylink.0"), []byte{1}, linkerULEB(uint32(len(memInfo))), memInfo)),
		linkerSection(1, linkerVec([]byte{0x60, 0, 0})),
		linkerSection(3, linkerVec([]byte{0})),
		linkerSection(7, linkerVec(linkerExport("_initialize", 0, 0))),
		linkerSection(10, linkerVec(linkerCode(append(init, 0x0b)...))),
	)
}

// linkerVariantSetup is linkerSetup for main module bin, in a runtime with
// features. It also returns the runtime.
func linkerVariantSetup(t *testing.T, features api.CoreFeatures, bin []byte) (context.Context, wazero.Runtime, api.Module, *Linker) {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCoreFeatures(features))
	t.Cleanup(func() { _ = r.Close(ctx) }) // the test is over; nothing reads its error
	main, err := r.InstantiateWithConfig(ctx, bin, wazero.NewModuleConfig().WithName("main"))
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCache(r)
	if err := cache.AddMain(bin); err != nil {
		t.Fatal(err)
	}
	l := NewLinker(cache)
	t.Cleanup(func() { _ = l.Close(ctx) }) // the runtime closes the modules anyway
	return ctx, r, main, l
}

func TestLinkerOpenVariantErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features api.CoreFeatures
		main     linkerMainVariant
		bin      []byte
		want     string
	}{
		{"no calloc", linkerFeatures, linkerMainVariant{noCalloc: true}, linkerBareSide(0), "undefined symbol: calloc"},
		{"calloc fails", linkerFeatures, linkerMainVariant{nullCalloc: true}, linkerBareSide(0), "out of memory for 16 bytes of data"},
		// The grow glue uses ref.null and table.grow, from reference
		// types, so it fails to compile.
		{"no reference types", api.CoreFeaturesV1, linkerMainVariant{}, linkerBareSide(0), `"reference-types" is disabled`},
		{"no table", linkerFeatures, linkerMainVariant{noTable: true}, linkerBareSide(0), "__indirect_function_table"},
		// The env glue imports main's memory for the side module.
		{"no memory", linkerFeatures, linkerMainVariant{noMemory: true}, linkerSideModule(true), "env glue:"},
		// env passes main's i32 on, which the side module takes for an i64.
		{"global type mismatch", linkerFeatures, linkerMainVariant{}, linkerSideModule(true, linkerImport("env", "main_data", 3, api.ValueTypeI64, 0)), "value type mismatch"},
		// A function whose address it takes, which neither module defines.
		{"undefined own function", linkerFeatures, linkerMainVariant{}, linkerSideModule(true, linkerImport("GOT.func", "nope", 3, api.ValueTypeI32, 1)), "undefined symbol: nope"},
		{"_initialize traps", linkerFeatures, linkerMainVariant{}, linkerBareSide(0, 0x00), "_initialize: wasm error: unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, main, l := linkerVariantSetup(t, tc.features, linkerVariantMain(tc.main))
			if _, err := l.Open(ctx, main, tc.bin); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Open = %v, want %q", err, tc.want)
			}
		})
	}
}

// A side module that needs table slots of its own gets them.
func TestLinkerOpenBare(t *testing.T) {
	ctx, _, main, l := linkerVariantSetup(t, linkerFeatures, linkerVariantMain(linkerMainVariant{}))
	h, err := l.Open(ctx, main, linkerBareSide(3))
	if err != nil {
		t.Fatal(err)
	}
	// The same binary again comes from the cache, and gets a new handle.
	again, err := l.Open(ctx, main, linkerBareSide(3))
	if err != nil {
		t.Fatal(err)
	}
	if h == again {
		t.Errorf("both opens got handle %d", h)
	}
}

// A wrapped libc function is the one a side module gets.
func TestLinkerOpenWrapped(t *testing.T) {
	ctx, _, main, l := linkerVariantSetup(t, linkerFeatures, linkerVariantMain(linkerMainVariant{wrapped: true}))
	h, err := l.Open(ctx, main, linkerSideModule(true))
	if err != nil {
		t.Fatal(err)
	}
	slot := linkerCall(t, ctx, l.libs[h].mod, "got_main_func")
	if got := linkerCall(t, ctx, main, "invoke", uint64(slot)); got != 8 {
		t.Errorf("GOT.func main_func calls into %d, want the wrapper's 8", got)
	}
}

// A tag main exports, as setjmp's __c_longjmp, links.
func TestLinkerOpenTag(t *testing.T) {
	ctx, _, main, l := linkerVariantSetup(t, linkerFeatures, linkerVariantMain(linkerMainVariant{tag: true}))
	if _, err := l.Open(ctx, main, linkerSideModule(true, linkerImport("env", "main_tag", 4, 0x00, 1))); err != nil {
		t.Fatal(err)
	}
	if l.cache.tagTypes["main_tag"] == nil {
		t.Errorf("tag types: %v", l.cache.tagTypes)
	}
}

func TestLinkerAddMainMalformed(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer func() { _ = r.Close(ctx) }() // nothing was compiled
	if err := NewCache(r).AddMain([]byte("not a module")); err == nil || err.Error() != "not a wasm module" {
		t.Errorf("AddMain = %v", err)
	}
}

// dlsym places a function in the table, which may be full by then.
func TestLinkerSymTableFull(t *testing.T) {
	// Open takes two slots, for the GOT.func entries, after main's one.
	ctx, main, l := linkerSetup(t, 3)
	h, err := l.Open(ctx, main, linkerSideModule(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Sym(ctx, main, h, "got_main_data"); err == nil || !strings.Contains(err.Error(), "cannot grow the function table") {
		t.Errorf("Sym = %v", err)
	}
}

// The grow glue is kept for every later call, so a call into it after it
// closed fails.
func TestLinkerGrowClosed(t *testing.T) {
	ctx, main, l := linkerSetup(t, 0)
	if _, err := l.growTable(ctx, main, 0); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.growTable(ctx, main, 1); err == nil {
		t.Error("growTable on a closed glue module succeeded")
	}
}

// Export registers the four host functions gophper_dl.c imports.
func TestLinkerExport(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer func() { _ = r.Close(ctx) }() // the host module needs nothing closed
	b := r.NewHostModuleBuilder("gophper")
	Export(b, func(context.Context) *Linker { return nil })
	m, err := b.Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range m.ExportedFunctionDefinitions() {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"dl_close", "dl_error", "dl_open", "dl_sym"}; !slices.Equal(names, want) {
		t.Errorf("exports %v, want %v", names, want)
	}
}
