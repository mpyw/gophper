//declscope:namespace main

package main

import (
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
// gophper, and Go's own. It is plain text, so that a review shows what
// changed.
//
//go:generate go run ./internal/genlicenses
//go:embed modlicenses.txt
var licensesModules string

// licensesFiles are the licenses of what php.wasm holds. A variable, so
// that a test can make reading them fail.
var licensesFiles fs.FS = phpwasm.Licenses

// licensesAction prints what a distributor of gophper must pass on.
func licensesAction(_ context.Context, cmd *cli.Command) error {
	w := cmd.Root().Writer
	if _, err := fmt.Fprintf(w, "gophper\n\n%s\n", gophper.License); err != nil {
		return err
	}
	entries, err := fs.ReadDir(licensesFiles, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fs.ReadFile(licensesFiles, e.Name())
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "\n---- %s ----\n\n%s\n", strings.TrimSuffix(e.Name(), ".txt"), b); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "\n"+licensesModules)
	return err
}
