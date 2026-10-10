//go:build !windows

//declscope:namespace process

package hostproc

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// processExhaustFDs leaves only free fds open to this process, under a lower
// limit so that few are needed. It returns a func that gives them back,
// to be called before anything else can need one, t.Error included.
func processExhaustFDs(t *testing.T, free int) func() {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	low := old
	low.Cur = min(old.Cur, 256)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	var fds []int
	release := func() {
		for _, fd := range fds {
			_ = syscall.Close(fd) // our own /dev/null fds
		}
		fds = nil
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) // back to what it was
	}
	for {
		fd, err := syscall.Open("/dev/null", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if errors.Is(err, syscall.EMFILE) {
			break
		}
		if err != nil {
			release()
			t.Fatal(err)
		}
		fds = append(fds, fd)
	}
	if len(fds) < free {
		release()
		t.Fatalf("only %d fds to free", len(fds))
	}
	for _, fd := range fds[len(fds)-free:] {
		_ = syscall.Close(fd) // our own /dev/null fds
	}
	fds = fds[:len(fds)-free]
	return release
}

// TestProcessSpawnNoFDs: a stdin that is no file is the null device to the
// child, which cannot be opened when no fd is left. No child starts.
func TestProcessSpawnNoFDs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := processTestRun{ctx: context.Background(), intr: make(chan struct{})}
	p := NewProcesses(run, nil, true, processTestHost, "", strings.NewReader("input"), nil, nil)
	mem := newProcessMemory(t)
	call := processSpawnCall{path: exe, argv: []string{"x"}, cwd: dir, fds: []processSpawnFD{{fd: 0, kind: processChildStdio, value: 0}}}
	release := processExhaustFDs(t, 0)
	got, _ := spawnProcess(t, p, mem, call)
	release()
	if got == 0 || len(p.children) != 0 {
		t.Errorf("errno %d, children %v", got, p.children)
	}
}
