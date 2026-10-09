//declscope:namespace process

package hostproc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mpyw/gophper/internal/wasi"
)

// processTestGuest is the path PHP would see for a host path: the same on
// Unix, and /c/... for C:\... on Windows.
func processTestGuest(host string) string {
	if runtime.GOOS != "windows" {
		return host
	}
	return "/" + strings.ToLower(host[:1]) + filepath.ToSlash(host[2:])
}

// processTestHost undoes processTestGuest.
func processTestHost(guest string) (string, bool, bool) {
	if runtime.GOOS != "windows" {
		return guest, true, true
	}
	if len(guest) < 3 || guest[0] != '/' || guest[2] != '/' {
		return "", false, false
	}
	return strings.ToUpper(guest[1:2]) + ":" + filepath.FromSlash(guest[2:]), true, true
}

// TestProcessSpawnFailures fails to start a child, on every OS, and leaves
// no pipe open behind. The program is this test binary, which every OS can
// run.
func TestProcessSpawnFailures(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := processTestRun{ctx: context.Background(), intr: make(chan struct{})}
	cwd := processTestGuest(dir)
	for _, tt := range []struct {
		name string
		call processSpawnCall
		want int32 // -1 for any error
	}{
		{"a directory as the program", processSpawnCall{path: cwd, argv: []string{"x"}, cwd: cwd}, -1},
		// The stdin pipe is set up first, then fd 3 fails: the pipe closes.
		{"a pipe, then a bad fd", processSpawnCall{path: processTestGuest(exe), argv: []string{"x"}, cwd: cwd, fds: []processSpawnFD{
			{fd: 0, kind: processChildStdio, value: 0},
			{fd: 3, kind: processChildStdio, value: 1},
		}}, wasi.EBADF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProcesses(run, nil, true, processTestHost, "", strings.NewReader("input"), nil, nil)
			got, _ := spawnProcess(t, p, newProcessMemory(t), tt.call)
			if (tt.want == -1 && got == 0) || (tt.want != -1 && got != tt.want) {
				t.Errorf("errno %d, want %d", got, tt.want)
			}
			if len(p.children) != 0 {
				t.Errorf("a child was started: %v", p.children)
			}
		})
	}
}
