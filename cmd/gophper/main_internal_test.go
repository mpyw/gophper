//declscope:namespace main

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/mpyw/gophper"
)

func TestGuestArgs(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "run.php")
	if err := os.WriteFile(abs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	in := []string{"-r", "echo 1;", abs, "run.php", "plain"}
	want := []string{"-r", "echo 1;", gophper.HostToGuest(abs), "run.php", "plain"}
	if runtime.GOOS == "windows" {
		// A relative path with backslashes takes slashes, if it names a file.
		if err := os.Mkdir("sub", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(`sub\x.php`, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		in = append(in, `sub\x.php`, `no\such`)
		want = append(want, "sub/x.php", `no\such`)
	}
	if got := guestArgs(in); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGuestEnv(t *testing.T) {
	if got := guestEnv([]string{"A=1", "TMPDIR=/x"}); !slices.Equal(got, []string{"A=1", "TMPDIR=/x"}) {
		t.Errorf("TMPDIR set: %q", got)
	}
	want := []string{"A=1", "TMPDIR=" + gophper.HostToGuest(os.TempDir())}
	if got := guestEnv([]string{"A=1"}); !slices.Equal(got, want) {
		t.Errorf("TMPDIR unset: %q, want %q", got, want)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": -1, "10": 10, "64M": 64 << 20, " 2g ": 2 << 30, "1k": 1 << 10} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "lots", "1T"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d", in, got)
		}
	}
}
