//declscope:namespace main

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
	"strings"

	phpwasm "github.com/mpyw/gophper-wasm"
	"github.com/urfave/cli/v3"

	"github.com/mpyw/gophper"
)

// licensesModules holds the license files of the Go modules linked into
// gophper, and Go's own, gzipped.
//
//go:generate go run ./internal/genlicenses
//go:embed modlicenses.txt.gz
var licensesModules []byte

// licensesAction prints what a distributor of gophper must pass on.
func licensesAction(_ context.Context, cmd *cli.Command) error {
	w := cmd.Root().Writer
	fmt.Fprintf(w, "gophper\n\n%s\n", gophper.License)
	entries, err := fs.ReadDir(phpwasm.Licenses, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fs.ReadFile(phpwasm.Licenses, e.Name())
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n---- %s ----\n\n%s\n", strings.TrimSuffix(e.Name(), ".txt"), b)
	}
	r, err := gzip.NewReader(bytes.NewReader(licensesModules))
	if err != nil {
		return err
	}
	fmt.Fprintln(w)
	_, err = io.Copy(w, r)
	return err
}
