package hostfn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
)

// Values cross between PHP and Go in a small binary encoding. gophper-wasm's
// compat/gophper_fn.c is the other side (ABI.md). Each value starts with a
// tag byte. Numbers are little-endian.
const (
	valueNull   byte = 'N'
	valueFalse  byte = 'F'
	valueTrue   byte = 'T'
	valueInt    byte = 'i' // int64
	valueFloat  byte = 'd' // float64
	valueString byte = 's' // u32 length, then the bytes
	valueArray  byte = 'a' // u32 count, then a key (int or string) and a value for each
)

var errValueShort = errors.New("hostfn: value cut short")

// valueDecode reads one value. A PHP array with the keys 0 to n-1, in order,
// is a []any. Any other array is a map[string]any, with int keys in decimal.
//
//declscope:shared // function.go decodes arguments with it
func valueDecode(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errValueShort
	}
	tag, b := b[0], b[1:]
	switch tag {
	case valueNull:
		return nil, b, nil
	case valueFalse:
		return false, b, nil
	case valueTrue:
		return true, b, nil
	case valueInt:
		if len(b) < 8 {
			return nil, nil, errValueShort
		}
		return int64(binary.LittleEndian.Uint64(b)), b[8:], nil
	case valueFloat:
		if len(b) < 8 {
			return nil, nil, errValueShort
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), b[8:], nil
	case valueString:
		if len(b) < 4 {
			return nil, nil, errValueShort
		}
		n := binary.LittleEndian.Uint32(b)
		b = b[4:]
		if uint32(len(b)) < n {
			return nil, nil, errValueShort
		}
		return string(b[:n]), b[n:], nil
	case valueArray:
		if len(b) < 4 {
			return nil, nil, errValueShort
		}
		n := binary.LittleEndian.Uint32(b)
		b = b[4:]
		keys := make([]any, 0, n)
		vals := make([]any, 0, n)
		list := true
		for i := range n {
			var k, v any
			var err error
			if k, b, err = valueDecode(b); err != nil {
				return nil, nil, err
			}
			if v, b, err = valueDecode(b); err != nil {
				return nil, nil, err
			}
			if ik, ok := k.(int64); !ok || ik != int64(i) {
				list = false
			}
			keys = append(keys, k)
			vals = append(vals, v)
		}
		if list {
			return vals, b, nil
		}
		m := make(map[string]any, n)
		for i, k := range keys {
			if ik, ok := k.(int64); ok {
				m[strconv.FormatInt(ik, 10)] = vals[i]
			} else {
				m[fmt.Sprint(k)] = vals[i]
			}
		}
		return m, b, nil
	}
	return nil, nil, fmt.Errorf("hostfn: unknown value tag %q", tag)
}

// valueEncode appends v. Go values map to PHP ones: nil, bools, integers,
// floats, strings and []byte, slices and arrays as lists, and maps with
// string or integer keys, in sorted key order.
//
//declscope:shared // function.go encodes results with it
func valueEncode(out []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(out, valueNull), nil
	case bool:
		if x {
			return append(out, valueTrue), nil
		}
		return append(out, valueFalse), nil
	case string:
		return valueEncodeString(out, x), nil
	case []byte:
		return valueEncodeString(out, string(x)), nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return binary.LittleEndian.AppendUint64(append(out, valueInt), uint64(rv.Int())), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return nil, fmt.Errorf("hostfn: %d does not fit in a PHP int", u)
		}
		return binary.LittleEndian.AppendUint64(append(out, valueInt), u), nil
	case reflect.Float32, reflect.Float64:
		return binary.LittleEndian.AppendUint64(append(out, valueFloat), math.Float64bits(rv.Float())), nil
	case reflect.String:
		return valueEncodeString(out, rv.String()), nil
	case reflect.Slice, reflect.Array:
		out = binary.LittleEndian.AppendUint32(append(out, valueArray), uint32(rv.Len()))
		for i := range rv.Len() {
			out = binary.LittleEndian.AppendUint64(append(out, valueInt), uint64(i))
			var err error
			if out, err = valueEncode(out, rv.Index(i).Interface()); err != nil {
				return nil, err
			}
		}
		return out, nil
	case reflect.Map:
		keys := rv.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int {
			return valueCmpKey(a, b)
		})
		out = binary.LittleEndian.AppendUint32(append(out, valueArray), uint32(len(keys)))
		for _, k := range keys {
			switch k.Kind() {
			case reflect.String:
				out = valueEncodeString(out, k.String())
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				out = binary.LittleEndian.AppendUint64(append(out, valueInt), uint64(k.Int()))
			default:
				return nil, fmt.Errorf("hostfn: a PHP array key cannot be a %s", k.Type())
			}
			var err error
			if out, err = valueEncode(out, rv.MapIndex(k).Interface()); err != nil {
				return nil, err
			}
		}
		return out, nil
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return append(out, valueNull), nil
		}
		return valueEncode(out, rv.Elem().Interface())
	}
	return nil, fmt.Errorf("hostfn: a %T cannot be a PHP value", v)
}

func valueEncodeString(out []byte, s string) []byte {
	out = binary.LittleEndian.AppendUint32(append(out, valueString), uint32(len(s)))
	return append(out, s...)
}

func valueCmpKey(a, b reflect.Value) int {
	if a.Kind() == reflect.String && b.Kind() == reflect.String {
		switch {
		case a.String() < b.String():
			return -1
		case a.String() > b.String():
			return 1
		}
		return 0
	}
	if a.CanInt() && b.CanInt() {
		switch {
		case a.Int() < b.Int():
			return -1
		case a.Int() > b.Int():
			return 1
		}
	}
	return 0
}
