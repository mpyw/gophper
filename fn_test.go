package gophper_test

import (
	"bytes"
	"context"
	"errors"
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
