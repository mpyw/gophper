//go:build windows

//declscope:namespace process

package hostproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mpyw/gophper/internal/wasi"
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

// TestProcessSpawnWindows refuses what Windows would run otherwise than
// checked, and a path on no drive.
func TestProcessSpawnWindows(t *testing.T) {
	dir := t.TempDir()
	bat := filepath.Join(dir, "run.bat")
	if err := os.WriteFile(bat, []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := processTestRun{ctx: context.Background(), intr: make(chan struct{})}
	cwd := processTestGuest(dir)
	for _, tt := range []struct {
		name string
		call processSpawnCall
		want int32
	}{
		{"a batch file with &", processSpawnCall{path: processTestGuest(filepath.Join(dir, "run")), argv: []string{"run", "x & calc"}, cwd: cwd}, wasi.EINVAL},
		{"no such program", processSpawnCall{path: processTestGuest(filepath.Join(dir, "missing")), argv: []string{"missing"}, cwd: cwd}, wasi.ENOENT},
		{"no drive", processSpawnCall{path: "/nodrive", argv: []string{"x"}, cwd: cwd}, wasi.ENOENT},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProcesses(run, nil, true, func(g string) (string, bool, bool) {
				if g == "/nodrive" {
					return "", false, false
				}
				return processTestHost(g)
			}, "", nil, nil, nil)
			if got, _ := spawnProcess(t, p, newProcessMemory(t), tt.call); got != tt.want {
				t.Errorf("errno %d, want %d", got, tt.want)
			}
		})
	}
}

// TestProcessShellWindows runs a command line with a sh.exe from PATH, or
// with cmd.exe when there is none.
func TestProcessShellWindows(t *testing.T) {
	if _, ok := processShell("/bin/ls", []string{"ls"}); ok {
		t.Error("not a shell command, but taken for one")
	}
	if sh, err := exec.LookPath("sh.exe"); err == nil {
		cmd, ok := processShell("/bin/sh", []string{"sh", "-c", "echo hi"})
		if !ok || cmd.Path != sh {
			t.Errorf("with sh.exe: %v, %v", cmd, ok)
		}
	}
	t.Setenv("PATH", "")
	t.Setenv("ComSpec", "")
	cmd, ok := processShell("/bin/sh", []string{"sh", "-c", "echo hi"})
	if !ok || !strings.EqualFold(filepath.Base(cmd.Path), "cmd.exe") || !strings.Contains(cmd.SysProcAttr.CmdLine, `/s /c "echo hi"`) {
		t.Errorf("without sh: %+v, %v", cmd, ok)
	}
}

// TestProcessPathExtDefault uses Windows' own list when PATHEXT is empty.
func TestProcessPathExtDefault(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "prog.exe")
	if err := os.WriteFile(prog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATHEXT", "")
	if got, ok := processExecutable(filepath.Join(dir, "prog")); !ok || !strings.EqualFold(got, prog) {
		t.Errorf("%q, %v", got, ok)
	}
}
