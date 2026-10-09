// Package wasmbin reads and writes the parts of the WebAssembly binary
// format that dynamic linking needs: a module's imports, exports and
// dylink.0 section, and small modules built from scratch.
package wasmbin

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// Module is what linking needs from a module's binary.
type Module struct {
	// Dylink reports a dylink.0 section, which every side module has.
	Dylink bool
	// From dylink.0: how much memory and how many table slots the module needs.
	// MemAlign is a power of two.
	MemSize, MemAlign, TableSize uint32
	Imports                      []Import
	// ExportedGlobalInit maps an exported global to its i32.const init value.
	// For a side module, that is a data symbol's offset from __memory_base.
	ExportedGlobalInit map[string]uint32
	// TagTypes maps an exported tag to its parameter types.
	TagTypes map[string][]api.ValueType
}

type Import struct {
	Module, Name string
	Kind         byte
}

// Import and export kinds.
const (
	KindFunc   = 0
	KindTable  = 1
	KindMemory = 2
	KindGlobal = 3
	KindTag    = 4
)

// Parse reads a module binary.
func Parse(b []byte) (info Module, err error) {
	defer func() {
		// The reader indexes past the end on a truncated module.
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed wasm module: %v", r)
		}
	}()
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return info, errors.New("not a wasm module")
	}
	info.ExportedGlobalInit = map[string]uint32{}
	info.TagTypes = map[string][]api.ValueType{}

	r := &reader{b: b, p: 8}
	var types [][]api.ValueType // parameter types of each function type
	var importedGlobals, importedTags uint32
	var importedTagTypes []uint32
	var globalInits []uint32
	var tagTypes []uint32
	globalExports := map[string]uint32{}
	tagExports := map[string]uint32{}

	for r.p < len(b) {
		id := r.byte()
		end := r.p + int(r.u32())
		switch id {
		case 0:
			if r.name() == "dylink.0" {
				info.Dylink = true
				for r.p < end {
					sub := r.byte()
					subEnd := r.p + int(r.u32())
					if sub == 1 { // WASM_DYLINK_MEM_INFO
						info.MemSize, info.MemAlign, info.TableSize = r.u32(), r.u32(), r.u32()
					}
					r.p = subEnd
				}
			}
		case 1:
			for n := r.u32(); n > 0; n-- {
				r.byte() // 0x60
				params := make([]api.ValueType, r.u32())
				for i := range params {
					params[i] = r.byte()
				}
				for m := r.u32(); m > 0; m-- {
					r.byte()
				}
				types = append(types, params)
			}
		case 2:
			for n := r.u32(); n > 0; n-- {
				imp := Import{Module: r.name(), Name: r.name(), Kind: r.byte()}
				switch imp.Kind {
				case KindFunc:
					r.u32()
				case KindTable:
					r.byte()
					r.limits()
				case KindMemory:
					r.limits()
				case KindGlobal:
					r.byte()
					r.byte()
					importedGlobals++
				case KindTag:
					r.byte()
					importedTagTypes = append(importedTagTypes, r.u32())
					importedTags++
				}
				info.Imports = append(info.Imports, imp)
			}
		case 6:
			for n := r.u32(); n > 0; n-- {
				r.byte() // value type
				r.byte() // mutability
				var v uint32
				if r.byte() == 0x41 { // i32.const
					v = uint32(r.s32())
				}
				for r.byte() != 0x0b {
				}
				globalInits = append(globalInits, v)
			}
		case 7:
			for n := r.u32(); n > 0; n-- {
				name := r.name()
				kind := r.byte()
				idx := r.u32()
				switch kind {
				case KindGlobal:
					globalExports[name] = idx
				case KindTag:
					tagExports[name] = idx
				}
			}
		case 13:
			for n := r.u32(); n > 0; n-- {
				r.byte() // attribute
				tagTypes = append(tagTypes, r.u32())
			}
		}
		r.p = end
	}

	for name, idx := range globalExports {
		if idx >= importedGlobals && int(idx-importedGlobals) < len(globalInits) {
			info.ExportedGlobalInit[name] = globalInits[idx-importedGlobals]
		}
	}
	for name, idx := range tagExports {
		var t uint32
		if idx < importedTags {
			t = importedTagTypes[idx]
		} else {
			t = tagTypes[idx-importedTags]
		}
		info.TagTypes[name] = types[t]
	}
	return info, nil
}

type reader struct {
	b []byte
	p int
}

func (r *reader) byte() byte { v := r.b[r.p]; r.p++; return v }

func (r *reader) u32() uint32 {
	v, n := binary.Uvarint(r.b[r.p:])
	if n <= 0 {
		panic("bad LEB128")
	}
	r.p += n
	return uint32(v)
}

func (r *reader) s32() int32 {
	var result int32
	var shift uint
	for {
		b := r.byte()
		result |= int32(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			if shift < 32 && b&0x40 != 0 {
				result |= -1 << shift
			}
			return result
		}
	}
}

func (r *reader) name() string {
	n := int(r.u32())
	s := string(r.b[r.p : r.p+n])
	r.p += n
	return s
}

func (r *reader) limits() {
	if r.byte()&1 != 0 {
		r.u32()
	}
	r.u32()
}

// Builder encodes a small module, such as the glue a dynamic linker generates.
type Builder struct {
	types, imports, funcs, globals, exports, elems, codes [][]byte

	importedFuncs, importedGlobals, importedTags uint32
	hasTable                                     bool
}

func (b *Builder) funcType(params, results []api.ValueType) uint32 {
	t := []byte{0x60}
	t = append(t, valueTypes(params)...)
	t = append(t, valueTypes(results)...)
	for i, existing := range b.types {
		if string(existing) == string(t) {
			return uint32(i)
		}
	}
	b.types = append(b.types, t)
	return uint32(len(b.types) - 1)
}

// ImportFunc imports a function and returns its index.
func (b *Builder) ImportFunc(module, name string, params, results []api.ValueType) uint32 {
	e := importHeader(module, name, KindFunc)
	b.imports = append(b.imports, append(e, uleb(b.funcType(params, results))...))
	b.importedFuncs++
	return b.importedFuncs - 1
}

// ImportTable imports a funcref table, as table 0. Minimum 0 matches any
// table. A second call does nothing.
func (b *Builder) ImportTable(module, name string) {
	if b.hasTable {
		return
	}
	b.hasTable = true
	b.imports = append(b.imports, append(importHeader(module, name, KindTable), 0x70, 0x00, 0x00))
}

// ImportMemory imports a memory. Minimum 0 matches any memory.
func (b *Builder) ImportMemory(module, name string) {
	b.imports = append(b.imports, append(importHeader(module, name, KindMemory), 0x00, 0x00))
}

// ImportGlobal imports a global and returns its index.
func (b *Builder) ImportGlobal(module, name string, typ api.ValueType, mutable bool) uint32 {
	e := append(importHeader(module, name, KindGlobal), typ, boolByte(mutable))
	b.imports = append(b.imports, e)
	b.importedGlobals++
	return b.importedGlobals - 1
}

// ImportTag imports an exception tag and returns its index.
func (b *Builder) ImportTag(module, name string, params []api.ValueType) uint32 {
	e := append(importHeader(module, name, KindTag), 0x00)
	b.imports = append(b.imports, append(e, uleb(b.funcType(params, nil))...))
	b.importedTags++
	return b.importedTags - 1
}

// DefineGlobal adds an i32 global and returns its index.
func (b *Builder) DefineGlobal(mutable bool, v uint32) uint32 {
	g := []byte{api.ValueTypeI32, boolByte(mutable), 0x41}
	g = append(g, sleb(int32(v))...)
	b.globals = append(b.globals, append(g, 0x0b))
	return b.importedGlobals + uint32(len(b.globals)) - 1
}

// DefineGrow adds a function (n i32) i32 that grows table 0 by n null
// entries and returns its old size.
func (b *Builder) DefineGrow() uint32 {
	b.funcs = append(b.funcs, uleb(b.funcType([]api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32})))
	body := []byte{0x00, 0xd0, 0x70, 0x20, 0x00, 0xfc, 0x0f, 0x00, 0x0b}
	b.codes = append(b.codes, append(uleb(uint32(len(body))), body...))
	return b.importedFuncs + uint32(len(b.funcs)) - 1
}

// DefineTrampoline adds a function of the given type that calls whatever
// table 0 holds at the index in global slot, and returns its index. The
// slot can be filled after the module that defines the target is
// instantiated. Table 0 must be imported.
func (b *Builder) DefineTrampoline(params, results []api.ValueType, slot uint32) uint32 {
	typ := b.funcType(params, results)
	b.funcs = append(b.funcs, uleb(typ))
	body := []byte{0x00} // no locals
	for i := range params {
		body = append(body, 0x20)
		body = append(body, uleb(uint32(i))...)
	}
	body = append(body, 0x23)
	body = append(body, uleb(slot)...)
	body = append(body, 0x11)
	body = append(body, uleb(typ)...)
	body = append(body, 0x00, 0x0b)
	b.codes = append(b.codes, append(uleb(uint32(len(body))), body...))
	return b.importedFuncs + uint32(len(b.funcs)) - 1
}

// Export exports the entity of kind at idx.
func (b *Builder) Export(name string, kind byte, idx uint32) {
	b.exports = append(b.exports, append(append(nameBytes(name), kind), uleb(idx)...))
}

// Elem places function fn at offset in table 0.
func (b *Builder) Elem(offset, fn uint32) {
	e := append([]byte{0x00, 0x41}, sleb(int32(offset))...)
	e = append(e, 0x0b, 0x01)
	b.elems = append(b.elems, append(e, uleb(fn)...))
}

// Encode returns the module binary.
func (b *Builder) Encode() []byte {
	out := []byte("\x00asm\x01\x00\x00\x00")
	section := func(id byte, items [][]byte) {
		if len(items) == 0 {
			return
		}
		body := uleb(uint32(len(items)))
		for _, it := range items {
			body = append(body, it...)
		}
		out = append(out, id)
		out = append(out, uleb(uint32(len(body)))...)
		out = append(out, body...)
	}
	section(1, b.types)
	section(2, b.imports)
	section(3, b.funcs)
	section(6, b.globals)
	section(7, b.exports)
	section(9, b.elems)
	section(10, b.codes)
	return out
}

func importHeader(module, name string, kind byte) []byte {
	return append(append(nameBytes(module), nameBytes(name)...), kind)
}

func valueTypes(vs []api.ValueType) []byte {
	return append(uleb(uint32(len(vs))), vs...)
}

func nameBytes(s string) []byte { return append(uleb(uint32(len(s))), s...) }

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func uleb(v uint32) []byte {
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

func sleb(v int32) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		done := (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0)
		if !done {
			c |= 0x80
		}
		out = append(out, c)
		if done {
			return out
		}
	}
}
