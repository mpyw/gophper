package gophper_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

func TestFunctions(t *testing.T) {
	var out bytes.Buffer
	exit, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args: []string{"-r", `
			var_export(go_add(20, 22)); echo "\n";
			var_export(go_echo(null, true, 1.5, "bin\0ary", [1, 2], ["a" => ["b" => 3]])); echo "\n";
			try { go_fail(); } catch (RuntimeException $e) { echo get_class($e), ": ", $e->getMessage(), "\n"; }
			echo GO_ADD(1, 2), " ", function_exists("go_add") ? "exists" : "missing", "\n";
		`},
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig(),
		Functions: map[string]gophper.Function{
			"go_add": func(_ context.Context, args []any) (any, error) {
				return args[0].(int64) + args[1].(int64), nil
			},
			"go_echo": func(_ context.Context, args []any) (any, error) { return args, nil },
			"go_fail": func(context.Context, []any) (any, error) { return nil, errors.New("from Go") },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"42",
		"array (\n  0 => NULL,\n  1 => true,\n  2 => 1.5,\n  3 => 'bin' . \"\\0\" . 'ary',\n  4 => \n  array (\n    0 => 1,\n    1 => 2,\n  ),\n  5 => \n  array (\n    'a' => \n    array (\n      'b' => 3,\n    ),\n  ),\n)",
		"RuntimeException: from Go",
		"3 exists",
		"",
	}, "\n")
	if exit != 0 || out.String() != want {
		t.Errorf("exit %d\n%s", exit, out.String())
	}
}

// Go values of other types reach PHP as the matching PHP values, and PHP
// arrays with mixed keys reach Go as maps.
func TestFunctionsValueTypes(t *testing.T) {
	type label string
	var got []any
	var out bytes.Buffer
	exit, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args: []string{"-r", `
			var_export(go_values()); echo "\n";
			var_export(go_mixed()); echo "\n";
			var_export(go_keep([5 => "five", "x" => [1 => "a", 0 => "b"], 0 => 1.0, "n" => null]) ); echo "\n";
			try { go_bad(); } catch (RuntimeException $e) { echo $e->getMessage(), "\n"; }
		`},
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig(),
		Functions: map[string]gophper.Function{
			"go_values": func(context.Context, []any) (any, error) {
				n := 7
				return []any{uint8(255), float32(0.5), label("named"), []byte("raw"), &n, (*int)(nil), [2]bool{true, false}}, nil
			},
			"go_mixed": func(context.Context, []any) (any, error) {
				return map[any]any{"b": map[int]string{2: "two", -1: "minus"}, 10: "ten", "a": []string{"x"}, 3: nil}, nil
			},
			"go_keep": func(_ context.Context, args []any) (any, error) {
				got = args
				return args[0], nil
			},
			"go_bad": func(context.Context, []any) (any, error) { return map[float64]int{1.5: 1}, nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"array (\n  0 => 255,\n  1 => 0.5,\n  2 => 'named',\n  3 => 'raw',\n  4 => 7,\n  5 => NULL,\n  6 => \n  array (\n    0 => true,\n    1 => false,\n  ),\n)",
		"array (\n  3 => NULL,\n  10 => 'ten',\n  'a' => \n  array (\n    0 => 'x',\n  ),\n  'b' => \n  array (\n    -1 => 'minus',\n    2 => 'two',\n  ),\n)",
		"array (\n  0 => 1.0,\n  5 => 'five',\n  'n' => NULL,\n  'x' => \n  array (\n    0 => 'b',\n    1 => 'a',\n  ),\n)",
		"hostfn: a PHP array key cannot be a float64",
		"",
	}, "\n")
	if exit != 0 || out.String() != want {
		t.Errorf("exit %d\n%s", exit, out.String())
	}
	wantArg := map[string]any{"5": "five", "x": map[string]any{"1": "a", "0": "b"}, "0": 1.0, "n": nil}
	if len(got) != 1 || !reflect.DeepEqual(got[0], wantArg) {
		t.Errorf("Go got %#v", got)
	}
}
