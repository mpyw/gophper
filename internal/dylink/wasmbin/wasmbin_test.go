package wasmbin

import (
	"slices"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// wasmbinSection encodes one section with its id and size.
func wasmbinSection(id byte, body ...[]byte) []byte {
	b := slices.Concat(body...)
	return slices.Concat([]byte{id}, uleb(uint32(len(b))), b)
}

func TestParseImportLimits(t *testing.T) {
	imports := slices.Concat(
		uleb(4),
		// A table with a maximum: flags 1, min 1, max 300 (two bytes).
		importHeader("env", "table", KindTable), []byte{0x70, 0x01, 0x01}, uleb(300),
		// A memory with a maximum: flags 1, min 2, max 70000 (three bytes).
		importHeader("env", "memory", KindMemory), []byte{0x01, 0x02}, uleb(70000),
		// A memory without one.
		importHeader("env", "memory2", KindMemory), []byte{0x00, 0x01},
		importHeader("env", "after", KindGlobal), []byte{api.ValueTypeI32, 0x00},
	)
	bin := slices.Concat([]byte("\x00asm\x01\x00\x00\x00"), wasmbinSection(2, imports))
	info, err := Parse(bin)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, imp := range info.Imports {
		got = append(got, imp.Module+"."+imp.Name)
	}
	if want := []string{"env.table", "env.memory", "env.memory2", "env.after"}; !slices.Equal(got, want) {
		t.Errorf("imports = %v, want %v", got, want)
	}
}

func TestParseDylink(t *testing.T) {
	memInfo := slices.Concat(uleb(1024), uleb(4), uleb(3), uleb(0))
	custom := slices.Concat(
		nameBytes("dylink.0"),
		// An unknown subsection is skipped.
		[]byte{9}, uleb(2), []byte{0xaa, 0xbb},
		[]byte{1}, uleb(uint32(len(memInfo))), memInfo,
	)
	bin := slices.Concat([]byte("\x00asm\x01\x00\x00\x00"),
		wasmbinSection(0, custom),
		// Another custom section is not dylink.0.
		wasmbinSection(0, nameBytes("name"), []byte{1, 2, 3}),
	)
	info, err := Parse(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Dylink || info.MemSize != 1024 || info.MemAlign != 4 || info.TableSize != 3 {
		t.Errorf("got %+v", info)
	}
}

// Data symbols are i32.const globals. Their offsets may use every bit,
// which the signed LEB128 encoding spreads over five bytes.
func TestParseExportedGlobalInit(t *testing.T) {
	b := &Builder{}
	b.ImportGlobal("env", "imported", api.ValueTypeI32, false)
	values := map[string]uint32{"zero": 0, "small": 8, "high": 0x8000_0000, "max": 0xffff_ffff, "negative": 0xffff_fff0}
	for name, v := range values {
		b.Export(name, KindGlobal, b.DefineGlobal(false, v))
	}
	// An export of an imported global has no init value.
	b.Export("reexport", KindGlobal, 0)
	info, err := Parse(b.Encode())
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range values {
		if got, ok := info.ExportedGlobalInit[name]; !ok || got != v {
			t.Errorf("%s = %#x, %v, want %#x", name, got, ok, v)
		}
	}
	if _, ok := info.ExportedGlobalInit["reexport"]; ok {
		t.Error("an imported global has an init value")
	}
}

func TestParseTags(t *testing.T) {
	b := &Builder{}
	// A function type before the tag's, with results to skip.
	b.ImportFunc("env", "f", []api.ValueType{api.ValueTypeI64}, []api.ValueType{api.ValueTypeI32, api.ValueTypeI32})
	b.Export("imported_tag", KindTag, b.ImportTag("env", "__c_longjmp", []api.ValueType{api.ValueTypeI32}))
	info, err := Parse(b.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.TagTypes["imported_tag"]; !slices.Equal(got, []api.ValueType{api.ValueTypeI32}) {
		t.Errorf("imported tag params = %v", got)
	}

	// A tag the module defines, in the tag section (13).
	types := slices.Concat(uleb(1), []byte{0x60}, valueTypes([]api.ValueType{api.ValueTypeI64, api.ValueTypeF32}), valueTypes(nil))
	tags := slices.Concat(uleb(1), []byte{0x00}, uleb(0))
	exports := slices.Concat(uleb(1), nameBytes("own_tag"), []byte{KindTag}, uleb(0))
	bin := slices.Concat([]byte("\x00asm\x01\x00\x00\x00"), wasmbinSection(1, types), wasmbinSection(13, tags), wasmbinSection(7, exports))
	info, err = Parse(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.TagTypes["own_tag"]; !slices.Equal(got, []api.ValueType{api.ValueTypeI64, api.ValueTypeF32}) {
		t.Errorf("own tag params = %v", got)
	}
}

func TestParseMalformed(t *testing.T) {
	header := []byte("\x00asm\x01\x00\x00\x00")
	for name, bin := range map[string][]byte{
		"empty":                {},
		"not wasm":             []byte("\x7fELF\x02\x01\x01\x00"),
		"section size cut":     slices.Concat(header, []byte{2, 0x80}),
		"section size too big": slices.Concat(header, []byte{2, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}),
		"import cut":           slices.Concat(header, []byte{2, 10, 1, 3, 'e', 'n'}),
		"global init cut":      slices.Concat(header, []byte{6, 4, 1, api.ValueTypeI32, 0, 0x41}),
	} {
		_, err := Parse(bin)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if name != "empty" && name != "not wasm" && !strings.Contains(err.Error(), "malformed wasm module") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
