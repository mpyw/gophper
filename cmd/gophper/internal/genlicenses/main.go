// Command genlicenses writes modlicenses.txt for gophper licenses: the
// license files of every Go module linked into cmd/gophper, and Go's own.
// Run it with go generate after go.mod changes.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// systems and arches are the builds covered. Some modules, such as
// golang.org/x/sys/windows, are linked on one system only.
var (
	systems = []string{"linux", "darwin", "windows", "freebsd"}
	arches  = []string{"amd64", "arm64"}
)

var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([.-].*)?$`)

type module struct {
	path, version, dir string
}

func main() {
	if err := run(); err != nil {
		logf(os.Stderr, "genlicenses: %v\n", err)
		os.Exit(1)
	}
}

// logf writes a diagnostic. A failed write to stderr has nowhere to be
// reported, so its error is dropped here.
func logf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func run() error {
	mods := map[string]module{}
	for _, goos := range systems {
		for _, goarch := range arches {
			if err := listModules(mods, goos, goarch); err != nil {
				return err
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

	return os.WriteFile("modlicenses.txt", buf.Bytes(), 0o644)
}

// listModules adds the modules linked into a build to mods. A go.work
// would change them, so it is ignored, as it is for go install.
func listModules(mods map[string]module, goos, goarch string) error {
	cmd := exec.Command("go", "list", "-deps", "-f",
		`{{with .Module}}{{if not .Main}}{{.Path}}	{{.Version}}	{{with .Replace}}{{.Dir}}{{else}}{{.Dir}}{{end}}{{end}}{{end}}`, ".")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "GOWORK=off")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list for %s/%s: %w", goos, goarch, err)
	}
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) == 3 {
			mods[f[0]] = module{f[0], f[1], f[2]}
		}
	}
	return nil
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
		if _, err := fmt.Fprintf(buf, "---- %s: %s ----\n\n%s\n\n", heading, e.Name(), bytes.TrimRight(b, "\n")); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("%s: no license file in %s", heading, dir)
	}
	return nil
}
