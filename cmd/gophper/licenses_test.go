//declscope:namespace main

package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLicensesModules fails when go.mod changed without go generate.
func TestLicensesModules(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	cmd := exec.Command("go", "list", "-deps", "-f",
		`{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}`, ".")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r, err := gzip.NewReader(bytes.NewReader(licensesModules))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(out)) {
		if m := strings.TrimSpace(line); m != "" && !bytes.Contains(b, []byte("---- "+m+": ")) {
			t.Errorf("no license for %s: run go generate ./cmd/gophper", m)
		}
	}
}
