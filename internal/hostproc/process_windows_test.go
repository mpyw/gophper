//go:build windows

//declscope:namespace process

package hostproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestProcessKillWindows fails a signal that Windows cannot send: only
// SIGKILL ends a process there. ping runs long enough to be signaled, and
// every Windows has it.
func TestProcessKillWindows(t *testing.T) {
	ping := filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE")
	if _, err := os.Stat(ping); err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	run := processTestRun{ctx: ctx, intr: make(chan struct{})}
	p := NewProcesses(run, nil, true, processTestHost, "", nil, nil, nil)
	m := newProcessMemory(t)
	x := processExports{from: func(context.Context) *Processes { return p }}
	errno, pid := spawnProcess(t, p, m, processSpawnCall{path: processTestGuest(ping), argv: []string{"ping", "-n", "30", "127.0.0.1"}, cwd: processTestGuest(t.TempDir())})
	if errno != 0 {
		t.Fatalf("spawn: errno %d", errno)
	}
	if got := x.kill(ctx, pid, 15); got == 0 {
		t.Error("SIGTERM was sent")
	}
	if got := x.kill(ctx, pid, 9); got != 0 {
		t.Errorf("SIGKILL: errno %d", got)
	}
	if got := x.wait(ctx, m, pid, 0, 64); got != pid {
		t.Errorf("wait: %d, want %d", got, pid)
	}
}
