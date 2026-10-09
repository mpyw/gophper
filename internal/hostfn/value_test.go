package hostfn

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// valueTestInt encodes an int as a value.
func valueTestInt(i int64) []byte {
	return binary.LittleEndian.AppendUint64([]byte{valueInt}, uint64(i))
}

type valueTestName string

func TestValueEncodeTypes(t *testing.T) {
	answer := 42
	var nilPtr *int
	var iface any = int8(-3)
	for _, c := range []struct {
		in   any
		want any // what decoding the encoding gives
	}{
		{nil, nil},
		{true, true},
		{false, false},
		{int8(-8), int64(-8)},
		{uint16(16), int64(16)},
		{uint64(math.MaxInt64), int64(math.MaxInt64)},
		{float32(1.5), 1.5},
		{"s", "s"},
		{[]byte("bytes"), "bytes"},
		{valueTestName("named"), "named"},
		{[2]int{1, 2}, []any{int64(1), int64(2)}},
		{[]string{}, []any{}},
		{&answer, int64(42)},
		{nilPtr, nil},
		{&iface, int64(-3)},
		{map[int]string{2: "b", -1: "a"}, map[string]any{"-1": "a", "2": "b"}},
		{map[int8]bool{0: true, 1: false}, []any{true, false}},
		{map[string]any{"z": nil, "a": []any{map[string]any{"deep": 1.0}}}, map[string]any{"z": nil, "a": []any{map[string]any{"deep": 1.0}}}},
		{map[valueTestName]int{"k": 1}, map[string]any{"k": int64(1)}},
		// Mixed keys, as PHP arrays have them.
		{map[any]any{"x": 1, 0: "zero", 1: "one"}, map[string]any{"0": "zero", "1": "one", "x": int64(1)}},
		{map[any]any{1: "one", 0: "zero"}, []any{"zero", "one"}},
	} {
		b, err := valueEncode(nil, c.in)
		if err != nil {
			t.Errorf("valueEncode(%#v): %v", c.in, err)
			continue
		}
		got, rest, err := valueDecode(b)
		if err != nil || len(rest) != 0 || !reflect.DeepEqual(got, c.want) {
			t.Errorf("valueEncode(%#v) decodes to %#v, %q, %v; want %#v", c.in, got, rest, err, c.want)
		}
	}
}

// Keys go in a fixed order, integers first, so a result is the same each run.
func TestValueEncodeKeyOrder(t *testing.T) {
	b, err := valueEncode(nil, map[any]any{"b": 0, 10: 0, "a": 0, -5: 0, 2: 0})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for b = b[5:]; len(b) > 0; {
		var k any
		k, b, _ = valueDecode(b)
		_, b, _ = valueDecode(b)
		keys = append(keys, fmt.Sprintf("%T:%v", k, k))
	}
	if got := strings.Join(keys, ","); got != "int64:-5,int64:2,int64:10,string:a,string:b" {
		t.Errorf("key order %s", got)
	}
}

func TestValueEncodeErrors(t *testing.T) {
	for _, in := range []any{
		uint64(math.MaxInt64) + 1,
		make(chan int),
		func() {},
		map[float64]int{1.5: 1},
		map[any]any{1.5: 1},
		map[any]any{nil: 1},
		map[uint]int{1: 1},
		[]any{1, make(chan int)},
		map[string]any{"k": struct{}{}},
	} {
		if b, err := valueEncode(nil, in); err == nil {
			t.Errorf("valueEncode(%T) = % x, want an error", in, b)
		}
	}
}

func TestValueDecodeErrors(t *testing.T) {
	arr := func(n uint32, rest ...byte) []byte {
		return append(binary.LittleEndian.AppendUint32([]byte{valueArray}, n), rest...)
	}
	for name, b := range map[string][]byte{
		"empty":        nil,
		"short int":    {valueInt, 1, 2},
		"short float":  {valueFloat, 1},
		"short length": {valueString, 1},
		"short string": append(binary.LittleEndian.AppendUint32([]byte{valueString}, 5), "abc"...),
		"short count":  {valueArray, 0},
		"no key":       arr(1),
		"no value":     arr(1, valueTestInt(0)...),
		"huge count":   arr(1<<32-1, valueNull, valueNull),
		"unknown tag":  {'?'},
	} {
		if v, _, err := valueDecode(b); err == nil {
			t.Errorf("%s: decoded %#v", name, v)
		}
	}
	// Keys out of order, or not integers, make a map.
	b := arr(2, append(append(valueTestInt(1), valueTrue), append(valueTestInt(0), valueFalse)...)...)
	if v, _, err := valueDecode(b); err != nil || !reflect.DeepEqual(v, map[string]any{"1": true, "0": false}) {
		t.Errorf("decoded %#v, %v", v, err)
	}
	b = arr(1, append([]byte{valueTrue}, valueNull)...)
	if v, _, err := valueDecode(b); err != nil || !reflect.DeepEqual(v, map[string]any{"true": nil}) {
		t.Errorf("decoded %#v, %v", v, err)
	}
}
