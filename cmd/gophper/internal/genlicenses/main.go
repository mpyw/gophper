// Command genlicenses writes modlicenses.txt.gz for gophper licenses: the
// license files of every Go module linked into cmd/gophper, and Go's own.
// Run it with go generate after go.mod changes.
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// systems are the systems whose builds are covered. Some modules,
// such as golang.org/x/sys/windows, are linked on one system only.
var systems = []string{"linux", "darwin", "windows", "freebsd"}

var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([.-].*)?$`)

type module struct {
	path, version, dir string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "genlicenses:", err)
		os.Exit(1)
	}
}

func run() error {
	mods := map[string]module{}
	for _, goos := range systems {
		cmd := exec.Command("go", "list", "-deps", "-f",
			`{{with .Module}}{{if not .Main}}{{.Path}}	{{.Version}}	{{with .Replace}}{{.Dir}}{{else}}{{.Dir}}{{end}}{{end}}{{end}}`, ".")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("go list for %s: %w", goos, err)
		}
		for line := range strings.Lines(string(out)) {
			f := strings.Split(strings.TrimSpace(line), "\t")
			if len(f) == 3 {
				mods[f[0]] = module{f[0], f[1], f[2]}
			}
		}
	}

	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := appendLicenses(&buf, "Go (the standard library and runtime)", strings.TrimSpace(string(goroot))); err != nil {
		return err
	}
	paths := make([]string, 0, len(mods))
	for p := range mods {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		m := mods[p]
		if err := appendLicenses(&buf, m.path+" "+m.version, m.dir); err != nil {
			return err
		}
	}

	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(buf.Bytes())
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile("modlicenses.txt.gz", gz.Bytes(), 0o644)
}

// appendLicenses writes the license files at the root of dir under a
// heading. A module without one is an error, as its terms are unknown.
func appendLicenses(buf *bytes.Buffer, heading, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var found bool
	for _, e := range entries {
		if e.IsDir() || !licenseFile.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "---- %s: %s ----\n\n%s\n", heading, e.Name(), bytes.TrimRight(b, "\n"))
		buf.WriteString("\n")
		found = true
	}
	if !found {
		return fmt.Errorf("%s: no license file in %s", heading, dir)
	}
	return nil
}
