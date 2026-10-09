//go:build windows

//declscope:namespace process

package hostproc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestProcessPathWindows reads Windows' Path key, keeps its spelling, and
// finds a program by name with PATHEXT, as Windows reports no execute bits.
func TestProcessPathWindows(t *testing.T) {
	if got := processWithBinDir([]string{"A=1", `Path=C:\Windows`}, `C:\bin`); !slices.Equal(got, []string{"A=1", `Path=C:\bin;C:\Windows`}) {
		t.Errorf("Path: %q", got)
	}
	if got := processWithBinDir([]string{"A=1"}, `C:\bin`); !slices.Equal(got, []string{"A=1", `PATH=C:\bin`}) {
		t.Errorf("no Path: %q", got)
	}
	dir := t.TempDir()
	prog := filepath.Join(dir, "prog.exe")
	if err := os.WriteFile(prog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATHEXT", ".COM;.EXE")
	// A name with a dot of its own still takes PATHEXT.
	dotted := filepath.Join(dir, "tool.v2.exe")
	if err := os.WriteFile(dotted, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"prog": prog, "prog.exe": prog, "tool.v2": dotted, "sub": "", "missing": ""} {
		got, err := processResolve(name, true, dir, []string{"Path=" + dir})
		if (want == "" && err == nil) || (want != "" && (err != nil || !os.SameFile(processStat(t, got), processStat(t, want)))) {
			t.Errorf("processResolve(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
}

func processStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestProcessBatchSafe refuses what cmd.exe would read in a batch file's
// arguments, and lets other programs have them.
func TestProcessBatchSafe(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{`C:\x\npm.cmd`, []string{"npm", "install", "left-pad"}, true},
		{`C:\x\npm.CMD`, []string{"npm", "install", "x & calc"}, false},
		{`C:\x\run.bat`, []string{"run", "100%"}, false},
		{`C:\x\run.bat`, []string{"run", "a\nb"}, false},
		{`C:\x\git.exe`, []string{"git", "commit", "-m", "a & b"}, true},
		{`C:\x\run.bat.`, []string{"run", "x & calc"}, false},
		{`C:\x\run.bat `, []string{"run", "x & calc"}, false},
		{`C:\x\tool.ps1`, []string{"tool", "x & calc"}, false},
	} {
		if got := processBatchSafe(tt.name, tt.args); got != tt.want {
			t.Errorf("processBatchSafe(%q, %q) = %v, want %v", tt.name, tt.args, got, tt.want)
		}
	}
}
