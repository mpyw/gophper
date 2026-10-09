// Package dylink links wasm side modules into a running php.wasm instance,
// as the dynamic loader links a .so.
//
// php.wasm is a static executable that exports every symbol, its function
// table and its stack pointer. A side module is built with -shared -fPIC and
// follows the WebAssembly dynamic linking convention (the dylink.0 section).
// The linker gives each side module its own region of memory and of the
// table, then generates small glue modules for the imports it needs:
//
//   - env: php.wasm's exports, plus the side module's __memory_base and
//     __table_base.
//   - GOT.mem: the address of each data symbol it uses.
//   - GOT.func: the table index of each function whose address it takes.
package dylink

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/mpyw/gophper/internal/dylink/wasmbin"
)

// Cache compiles side modules and glue modules once for every instance.
type Cache struct {
	runtime wazero.Runtime

	mu       sync.Mutex
	compiled map[[sha256.Size]byte]*linkerCompiled
	// tagTypes are the parameter types of the tags php.wasm exports, such
	// as setjmp's __c_longjmp.
	tagTypes map[string][]api.ValueType
}

// NewCache returns an empty cache for modules compiled by r.
func NewCache(r wazero.Runtime) *Cache {
	return &Cache{runtime: r, compiled: map[[sha256.Size]byte]*linkerCompiled{}, tagTypes: map[string][]api.ValueType{}}
}

// AddMain records what side modules may import from a main module binary.
func (c *Cache) AddMain(bin []byte) error {
	info, err := wasmbin.Parse(bin)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.Copy(c.tagTypes, info.TagTypes)
	return nil
}

// Linker links side modules into one main module instance.
// It is used by one PHP instance at a time.
type Linker struct {
	cache *Cache
	libs  map[int32]*linkerLibrary
	// modules are every module instantiated for this instance, to close.
	modules []api.Module
	grow    api.Function
	lastErr string
}

// NewLinker returns a linker for one PHP instance.
func NewLinker(cache *Cache) *Linker {
	return &Linker{cache: cache, libs: map[int32]*linkerLibrary{}}
}

// Open links bin into main and returns a handle for Sym.
func (l *Linker) Open(ctx context.Context, main api.Module, bin []byte) (int32, error) {
	side, err := l.cache.compile(ctx, bin)
	if err != nil {
		return 0, err
	}
	info := side.info
	if !info.Dylink {
		return 0, fmt.Errorf("not a shared library: no dylink.0 section")
	}

	// Memory comes from main's calloc, so data and bss start zeroed.
	align := uint32(1) << info.MemAlign
	raw, err := l.call(ctx, main, "calloc", 1, uint64(info.MemSize+align))
	if err != nil {
		return 0, err
	}
	if raw == 0 {
		return 0, fmt.Errorf("out of memory for %d bytes of data", info.MemSize)
	}
	memBase := (uint32(raw) + align - 1) &^ (align - 1)
	tableBase, err := l.growTable(ctx, main, info.TableSize)
	if err != nil {
		return 0, err
	}

	env := &wasmbin.Builder{}
	gotMem := &wasmbin.Builder{}
	gotFunc := &wasmbin.Builder{}
	var mainFuncs, ownFuncs, selfFuncs []string
	// Definitions in env come after every import: wasm numbers imports
	// first, so a definition's index is final only then.
	var envDefs []func()
	for _, imp := range info.Imports {
		switch imp.Module {
		case "env":
			def, self, err := l.linkEnv(env, imp, main, memBase, tableBase)
			if err != nil {
				return 0, err
			}
			if def != nil {
				envDefs = append(envDefs, def)
			}
			if self {
				selfFuncs = append(selfFuncs, imp.Name)
			}
		case "GOT.mem":
			addr, ok := l.dataAddress(main, imp.Name)
			if !ok {
				off, own := info.ExportedGlobalInit[imp.Name]
				if !own {
					return 0, fmt.Errorf("undefined symbol: %s", imp.Name)
				}
				addr = memBase + off
			}
			gotMem.Export(imp.Name, wasmbin.KindGlobal, gotMem.DefineGlobal(true, addr))
		case "GOT.func":
			// The table index is known only after placing the function, so
			// the GOT entry starts at 0 and is set below.
			gotFunc.Export(imp.Name, wasmbin.KindGlobal, gotFunc.DefineGlobal(true, 0))
			if linkerTarget(main, imp.Name) != "" {
				mainFuncs = append(mainFuncs, imp.Name)
			} else {
				ownFuncs = append(ownFuncs, imp.Name)
			}
		default:
			return 0, fmt.Errorf("unsupported import module %q", imp.Module)
		}
	}

	for _, def := range envDefs {
		def()
	}
	if len(selfFuncs) > 0 {
		types := map[string]api.FunctionDefinition{}
		for _, f := range side.mod.ImportedFunctions() {
			if module, name, _ := f.Import(); module == "env" {
				types[name] = f
			}
		}
		env.ImportTable("main", "__indirect_function_table")
		for _, name := range selfFuncs {
			f := types[name]
			slot := env.DefineGlobal(true, 0)
			env.Export(name, wasmbin.KindFunc, env.DefineTrampoline(f.ParamTypes(), f.ResultTypes(), slot))
			env.Export(linkerSlotPrefix+name, wasmbin.KindGlobal, slot)
		}
	}

	glue := map[string]api.Module{"main": main}
	for name, b := range map[string]*wasmbin.Builder{"env": env, "GOT.mem": gotMem, "GOT.func": gotFunc} {
		m, err := l.instantiateBytes(ctx, b.Encode(), glue)
		if err != nil {
			return 0, fmt.Errorf("%s glue: %w", name, err)
		}
		glue[name] = m
	}
	mod, err := l.instantiate(ctx, side.mod, glue)
	if err != nil {
		return 0, err
	}

	// Functions it imports from itself go through their trampolines.
	if len(selfFuncs) > 0 {
		slots, err := l.placeFuncs(ctx, main, mod, selfFuncs)
		if err != nil {
			return 0, err
		}
		for i, name := range selfFuncs {
			glue["env"].ExportedGlobal(linkerSlotPrefix + name).(api.MutableGlobal).Set(uint64(slots[i]))
		}
	}

	// Functions whose address the side module takes: main's, then its own.
	if err := l.setFuncGOT(ctx, main, glue["GOT.func"], main, mainFuncs); err != nil {
		return 0, err
	}
	if err := l.setFuncGOT(ctx, main, glue["GOT.func"], mod, ownFuncs); err != nil {
		return 0, err
	}

	for _, init := range []string{"__wasm_apply_data_relocs", "_initialize"} {
		if mod.ExportedFunction(init) != nil {
			if _, err := l.call(ctx, mod, init); err != nil {
				return 0, fmt.Errorf("%s: %w", init, err)
			}
		}
	}

	handle := int32(len(l.libs) + 1)
	l.libs[handle] = &linkerLibrary{mod: mod, memBase: memBase, funcSlots: map[string]uint32{}}
	return handle, nil
}

// Sym returns a symbol of a linked module as C sees it: the table index of
// a function, or the address of data.
func (l *Linker) Sym(ctx context.Context, main api.Module, handle int32, name string) (uint32, error) {
	lib := l.libs[handle]
	if lib == nil {
		return 0, fmt.Errorf("invalid handle %d", handle)
	}
	if slot, ok := lib.funcSlots[name]; ok {
		return slot, nil
	}
	if lib.mod.ExportedFunction(name) != nil {
		slots, err := l.placeFuncs(ctx, main, lib.mod, []string{name})
		if err != nil {
			return 0, err
		}
		lib.funcSlots[name] = slots[0]
		return slots[0], nil
	}
	if g := lib.mod.ExportedGlobal(name); g != nil {
		// Data symbols are exported relative to __memory_base.
		return lib.memBase + uint32(g.Get()), nil
	}
	return 0, fmt.Errorf("undefined symbol: %s", name)
}

// Close closes every module linked into the instance, and returns their
// errors joined.
func (l *Linker) Close(ctx context.Context) error {
	var err error
	for _, m := range slices.Backward(l.modules) {
		err = errors.Join(err, m.Close(ctx))
	}
	l.modules = nil
	return err
}

// linkerSlotPrefix names the global holding a trampoline's table slot.
const linkerSlotPrefix = "gophper.slot."

// linkEnv resolves one import from env. Imports are added at once. A
// definition is returned as def, to add after every import. self reports a
// function that main lacks: the side module itself defines it, weakly, and
// imports it so that another module could take its place. It goes through
// a trampoline, set once the side module exists.
func (l *Linker) linkEnv(env *wasmbin.Builder, imp wasmbin.Import, main api.Module, memBase, tableBase uint32) (def func(), self bool, err error) {
	switch {
	case imp.Name == "__memory_base":
		return func() { env.Export(imp.Name, wasmbin.KindGlobal, env.DefineGlobal(false, memBase)) }, false, nil
	case imp.Name == "__table_base":
		return func() { env.Export(imp.Name, wasmbin.KindGlobal, env.DefineGlobal(false, tableBase)) }, false, nil
	case imp.Kind == wasmbin.KindMemory:
		env.ImportMemory("main", "memory")
		env.Export(imp.Name, wasmbin.KindMemory, 0)
	case imp.Kind == wasmbin.KindTable:
		env.ImportTable("main", "__indirect_function_table")
		env.Export(imp.Name, wasmbin.KindTable, 0)
	case imp.Kind == wasmbin.KindGlobal:
		g := main.ExportedGlobal(imp.Name)
		if g == nil {
			return nil, false, fmt.Errorf("undefined symbol: %s", imp.Name)
		}
		_, mutable := g.(api.MutableGlobal)
		env.Export(imp.Name, wasmbin.KindGlobal, env.ImportGlobal("main", imp.Name, g.Type(), mutable))
	case imp.Kind == wasmbin.KindTag:
		params, ok := l.cache.tagTypes[imp.Name]
		if !ok {
			return nil, false, fmt.Errorf("undefined tag: %s", imp.Name)
		}
		env.Export(imp.Name, wasmbin.KindTag, env.ImportTag("main", imp.Name, params))
	case imp.Kind == wasmbin.KindFunc:
		target := linkerTarget(main, imp.Name)
		if target == "" {
			return nil, true, nil
		}
		fd := main.ExportedFunction(target).Definition()
		env.Export(imp.Name, wasmbin.KindFunc, env.ImportFunc("main", target, fd.ParamTypes(), fd.ResultTypes()))
	}
	return nil, false, nil
}

// linkerTarget is main's export that name resolves to, or "". gophper-wasm
// links some libc functions with --wrap, so that socket fds reach the host.
// A side module must call the wrapper, not the libc original.
func linkerTarget(main api.Module, name string) string {
	if main.ExportedFunction("__wrap_"+name) != nil {
		return "__wrap_" + name
	}
	if main.ExportedFunction(name) != nil {
		return name
	}
	return ""
}

func (l *Linker) dataAddress(main api.Module, name string) (uint32, bool) {
	if g := main.ExportedGlobal(name); g != nil {
		return uint32(g.Get()), true
	}
	return 0, false
}

// setFuncGOT places from's functions in the table and points the GOT.func
// entries at them.
func (l *Linker) setFuncGOT(ctx context.Context, main, got, from api.Module, names []string) error {
	if len(names) == 0 {
		return nil
	}
	targets := make([]string, len(names))
	for i, name := range names {
		targets[i] = name
		if from == main {
			targets[i] = linkerTarget(main, name)
		}
	}
	slots, err := l.placeFuncs(ctx, main, from, targets)
	if err != nil {
		return err
	}
	for i, name := range names {
		got.ExportedGlobal(name).(api.MutableGlobal).Set(uint64(slots[i]))
	}
	return nil
}

// placeFuncs puts from's exported functions into new table slots.
func (l *Linker) placeFuncs(ctx context.Context, main, from api.Module, names []string) ([]uint32, error) {
	base, err := l.growTable(ctx, main, uint32(len(names)))
	if err != nil {
		return nil, err
	}
	b := &wasmbin.Builder{}
	slots := make([]uint32, len(names))
	for i, name := range names {
		f := from.ExportedFunction(name)
		if f == nil {
			return nil, fmt.Errorf("undefined symbol: %s", name)
		}
		def := f.Definition()
		b.ImportFunc("from", name, def.ParamTypes(), def.ResultTypes())
		slots[i] = base + uint32(i)
	}
	b.ImportTable("main", "__indirect_function_table")
	for i := range names {
		b.Elem(slots[i], uint32(i))
	}
	if _, err := l.instantiateBytes(ctx, b.Encode(), map[string]api.Module{"main": main, "from": from}); err != nil {
		return nil, err
	}
	return slots, nil
}

func (l *Linker) growTable(ctx context.Context, main api.Module, n uint32) (uint32, error) {
	if l.grow == nil {
		b := &wasmbin.Builder{}
		b.ImportTable("main", "__indirect_function_table")
		b.Export("grow", wasmbin.KindFunc, b.DefineGrow())
		m, err := l.instantiateBytes(ctx, b.Encode(), map[string]api.Module{"main": main})
		if err != nil {
			return 0, err
		}
		l.grow = m.ExportedFunction("grow")
	}
	res, err := l.grow.Call(ctx, uint64(n))
	if err != nil {
		return 0, err
	}
	if int32(res[0]) < 0 {
		return 0, fmt.Errorf("cannot grow the function table by %d", n)
	}
	return uint32(res[0]), nil
}

func (l *Linker) instantiateBytes(ctx context.Context, bin []byte, imports map[string]api.Module) (api.Module, error) {
	c, err := l.cache.compile(ctx, bin)
	if err != nil {
		return nil, err
	}
	return l.instantiate(ctx, c.mod, imports)
}

func (l *Linker) instantiate(ctx context.Context, mod wazero.CompiledModule, imports map[string]api.Module) (api.Module, error) {
	ctx = experimental.WithImportResolver(ctx, func(name string) api.Module { return imports[name] })
	// No name, so that many instances can link at the same time.
	m, err := l.cache.runtime.InstantiateModule(ctx, mod, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, err
	}
	l.modules = append(l.modules, m)
	return m, nil
}

func (l *Linker) call(ctx context.Context, m api.Module, name string, args ...uint64) (uint64, error) {
	f := m.ExportedFunction(name)
	if f == nil {
		return 0, fmt.Errorf("undefined symbol: %s", name)
	}
	res, err := f.Call(ctx, args...)
	if err != nil || len(res) == 0 {
		return 0, err
	}
	return res[0], nil
}

// linkerLibrary is one linked side module.
type linkerLibrary struct {
	mod     api.Module
	memBase uint32
	// funcSlots caches the table index Sym gave each function.
	funcSlots map[string]uint32
}

type linkerCompiled struct {
	mod  wazero.CompiledModule
	info wasmbin.Module
}

func (c *Cache) compile(ctx context.Context, bin []byte) (*linkerCompiled, error) {
	key := sha256.Sum256(bin)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cm, ok := c.compiled[key]; ok {
		return cm, nil
	}
	info, err := wasmbin.Parse(bin)
	if err != nil {
		return nil, err
	}
	mod, err := c.runtime.CompileModule(context.WithoutCancel(ctx), bin)
	if err != nil {
		return nil, err
	}
	cm := &linkerCompiled{mod: mod, info: info}
	c.compiled[key] = cm
	return cm, nil
}

// Export adds the dlopen(3) host functions, which gophper-wasm's
// compat/gophper_dl.c calls, to b. from returns the calling instance's linker.
func Export(b wazero.HostModuleBuilder, from func(context.Context) *Linker) {
	x := linkerHostFuncs{from: from}
	b.NewFunctionBuilder().WithFunc(x.open).Export("dl_open")
	b.NewFunctionBuilder().WithFunc(x.sym).Export("dl_sym")
	b.NewFunctionBuilder().WithFunc(x.close).Export("dl_close")
	b.NewFunctionBuilder().WithFunc(x.error).Export("dl_error")
}

// linkerHostFuncs are the host functions. They return 0 on failure and leave a
// message for dl_error, as dlopen(3) leaves one for dlerror(3).
type linkerHostFuncs struct {
	from func(context.Context) *Linker
}

func (x linkerHostFuncs) open(ctx context.Context, m api.Module, ptr, n uint32) int32 {
	l := x.from(ctx)
	bin, ok := m.Memory().Read(ptr, n)
	if !ok {
		l.lastErr = "dlopen: out of bounds"
		return 0
	}
	h, err := l.Open(ctx, m, append([]byte(nil), bin...))
	if err != nil {
		l.lastErr = err.Error()
		return 0
	}
	return h
}

func (x linkerHostFuncs) sym(ctx context.Context, m api.Module, handle int32, namePtr, nameLen, valuePtr uint32) int32 {
	l := x.from(ctx)
	name, _ := m.Memory().Read(namePtr, nameLen)
	v, err := l.Sym(ctx, m, handle, string(name))
	if err != nil {
		l.lastErr = err.Error()
		return 0
	}
	m.Memory().WriteUint32Le(valuePtr, v)
	return 1
}

// close keeps the module linked until the instance ends: PHP unloads
// extensions only at shutdown.
func (linkerHostFuncs) close(context.Context, int32) int32 { return 1 }

func (x linkerHostFuncs) error(ctx context.Context, m api.Module, buf, capacity uint32) int32 {
	msg := x.from(ctx).lastErr
	if len(msg) > int(capacity) {
		msg = msg[:capacity]
	}
	m.Memory().WriteString(buf, msg)
	return int32(len(msg))
}
