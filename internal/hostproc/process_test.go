package hostproc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// processTestRun is a run whose interrupt and context the test controls.
type processTestRun struct {
	ctx  context.Context
	intr chan struct{}
}

func (r processTestRun) Context() context.Context      { return r.ctx }
func (r processTestRun) Interruption() <-chan struct{} { return r.intr }

// newProcessMemory returns a module with one page of linear memory, to call
// the host functions with as the guest would.
func newProcessMemory(t *testing.T) api.Module {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	// A module that defines only a memory of one page.
	m, err := r.Instantiate(ctx, []byte("\x00asm\x01\x00\x00\x00\x05\x03\x01\x00\x01"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// processSpawnFD is one struct gophper_child_fd.
type processSpawnFD struct {
	fd, kind, value, flags int32
	path                   string
}

// processSpawnCall lays out the arguments of proc_spawn in memory.
type processSpawnCall struct {
	path string
	argv []string
	cwd  string
	fds  []processSpawnFD
}

// spawnProcess calls proc_spawn as the guest would, and returns its errno
// and the pid it wrote.
func spawnProcess(t *testing.T, p *Processes, m api.Module, c processSpawnCall) (int32, int32) {
	t.Helper()
	mem := m.Memory()
	next := uint32(1024)
	put := func(s string) (uint32, uint32) {
		ptr := next
		mem.WriteString(ptr, s)
		next += uint32(len(s)) + 8
		return ptr, uint32(len(s))
	}
	pathPtr, pathLen := put(c.path)
	argv := ""
	for _, a := range c.argv {
		argv += a + "\x00"
	}
	argvPtr, argvLen := put(argv)
	envPtr, envLen := put("PATH=/usr/bin:/bin\x00")
	cwdPtr, cwdLen := put(c.cwd)
	fdsPtr := next
	next += uint32(len(c.fds)) * processChildFDSize
	for i, f := range c.fds {
		base := fdsPtr + uint32(i)*processChildFDSize
		fp, fl := put(f.path)
		for j, v := range []int32{f.fd, f.kind, f.value, f.flags, int32(fp), int32(fl)} {
			mem.WriteUint32Le(base+uint32(j)*4, uint32(v))
		}
	}
	pidPtr := next
	x := processExports{from: func(context.Context) *Processes { return p }}
	errno := x.spawn(context.Background(), m, pathPtr, pathLen, 0, argvPtr, argvLen, envPtr, envLen, cwdPtr, cwdLen, fdsPtr, int32(len(c.fds)), pidPtr)
	pid, _ := mem.ReadUint32Le(pidPtr)
	return errno, int32(pid)
}

// newProcessTable returns Processes that map every path to itself, and
// treat paths under readOnly as read-only. It has no sockets: tests may
// import only the public API, so host fds stay untested here.
func newProcessTable(run processTestRun, allowed bool, readOnly string) *Processes {
	hostPath := func(path string) (string, bool, bool) {
		if path == "/unmapped" || strings.HasPrefix(path, "/unmapped/") {
			return "", false, false
		}
		return path, readOnly == "" || !strings.HasPrefix(path, readOnly), true
	}
	return NewProcesses(run, nil, allowed, hostPath, "", nil, nil, nil)
}

func TestProcessSpawnRejects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	run := processTestRun{ctx: context.Background(), intr: make(chan struct{})}
	sh := processSpawnCall{path: "/bin/sh", argv: []string{"sh", "-c", "exit 0"}, cwd: dir}
	with := func(fds ...processSpawnFD) processSpawnCall {
		c := sh
		c.fds = fds
		return c
	}
	for _, tt := range []struct {
		name    string
		allowed bool
		call    processSpawnCall
		want    int32
	}{
		{"processes off", false, sh, wasi.EPERM},
		{"cwd without a host directory", true, processSpawnCall{path: "/bin/sh", cwd: "/unmapped"}, wasi.ENOENT},
		{"program not found", true, processSpawnCall{path: "/no/such/program", cwd: dir}, wasi.ENOENT},
		{"unknown fd kind", true, with(processSpawnFD{fd: 1, kind: 99}), wasi.EINVAL},
		{"unknown stdio", true, with(processSpawnFD{fd: 1, kind: processChildStdio, value: 7}), wasi.EBADF},
		{"stdio above stderr", true, with(processSpawnFD{fd: 3, kind: processChildStdio, value: 1}), wasi.EBADF},
		{"file without a host file", true, with(processSpawnFD{fd: 1, kind: processChildFile, flags: processOpenWrite, path: "/unmapped/f"}), wasi.EBADF},
		{"write to a read-only file", true, with(processSpawnFD{fd: 1, kind: processChildFile, flags: processOpenWrite | processOpenCreate, path: filepath.Join(ro, "f")}), wasi.EACCES},
		{"missing file", true, with(processSpawnFD{fd: 0, kind: processChildFile, flags: processOpenRead, path: filepath.Join(dir, "missing")}), wasi.ENOENT},
		{"exclusive on an existing file", true, with(processSpawnFD{fd: 1, kind: processChildFile, flags: processOpenWrite | processOpenCreate | processOpenExclusive, path: dir}), wasi.EIO},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newProcessTable(run, tt.allowed, ro)
			if got, _ := spawnProcess(t, p, newProcessMemory(t), tt.call); got != tt.want {
				t.Errorf("errno %d, want %d", got, tt.want)
			}
			if len(p.children) != 0 {
				t.Errorf("a child was started: %v", p.children)
			}
		})
	}
}

func TestProcessWaitAndKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := processTestRun{ctx: ctx, intr: make(chan struct{})}
	p := newProcessTable(run, true, "")
	m := newProcessMemory(t)
	x := processExports{from: func(context.Context) *Processes { return p }}
	const statusPtr = 64

	if got := x.wait(ctx, m, -1, 1, statusPtr); got != -wasi.ECHILD {
		t.Errorf("wait without children: %d, want -ECHILD", got)
	}
	if got := x.kill(ctx, 99999, 15); got != wasi.ESRCH {
		t.Errorf("kill of a stranger: %d, want ESRCH", got)
	}

	errno, pid := spawnProcess(t, p, m, processSpawnCall{path: "/bin/sh", argv: []string{"sh", "-c", "exec sleep 10"}, cwd: t.TempDir()})
	if errno != 0 {
		t.Fatalf("spawn: errno %d", errno)
	}
	if got := x.wait(ctx, m, pid, 1, statusPtr); got != 0 {
		t.Errorf("wait with WNOHANG on a running child: %d, want 0", got)
	}
	// A wait for one child is not satisfied by another one that has exited.
	errno, quick := spawnProcess(t, p, m, processSpawnCall{path: "/bin/sh", argv: []string{"sh", "-c", "exit 4"}, cwd: t.TempDir()})
	if errno != 0 {
		t.Fatalf("spawn: errno %d", errno)
	}
	if got := x.wait(ctx, m, quick, 0, statusPtr); got != quick {
		t.Fatalf("wait for the quick child: %d, want %d", got, quick)
	}
	if st, _ := m.Memory().ReadUint32Le(statusPtr); st != 4<<8 {
		t.Errorf("status %#x, want 4<<8", st)
	}
	if got := x.wait(ctx, m, pid, 1, statusPtr); got != 0 {
		t.Errorf("wait with WNOHANG on a running child: %d, want 0", got)
	}
	for _, tt := range []struct {
		sig, want int32
	}{
		{0, 0},            // only checks that the child exists
		{64, wasi.EINVAL}, // no such signal
		{9, 0},            // SIGKILL
	} {
		if got := x.kill(ctx, pid, tt.sig); got != tt.want {
			t.Errorf("kill(%d): %d, want %d", tt.sig, got, tt.want)
		}
	}
	// Once the child has exited, but before PHP reaps it, a signal is no error.
	for deadline := time.Now().Add(5 * time.Second); ; {
		p.mu.Lock()
		done := p.children[pid].done
		p.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child did not die")
		}
		time.Sleep(time.Millisecond)
	}
	if got := x.kill(ctx, pid, 15); got != 0 {
		t.Errorf("kill of an exited child: %d, want 0", got)
	}
	if got := x.wait(ctx, m, pid, 0, statusPtr); got != pid {
		t.Fatalf("wait: %d, want %d", got, pid)
	}
	if st, _ := m.Memory().ReadUint32Le(statusPtr); st != 9 {
		t.Errorf("status %#x, want 9 (killed by SIGKILL)", st)
	}

	// A wait cut short by an interrupt, then by the end of the run.
	if errno, _ = spawnProcess(t, p, m, processSpawnCall{path: "/bin/sh", argv: []string{"sh", "-c", "exec sleep 10"}, cwd: t.TempDir()}); errno != 0 {
		t.Fatalf("spawn: errno %d", errno)
	}
	close(run.intr)
	if got := x.wait(ctx, m, -1, 0, statusPtr); got != -wasi.EINTR {
		t.Errorf("interrupted wait: %d, want -EINTR", got)
	}
	run.intr = make(chan struct{})
	p.run = run
	cancel()
	if got := x.wait(ctx, m, -1, 0, statusPtr); got != -wasi.EIO {
		t.Errorf("wait after the run: %d, want -EIO", got)
	}
	for cpid := range p.children {
		x.kill(ctx, cpid, 9)
	}
}

func TestProcessOpenFlags(t *testing.T) {
	for _, tt := range []struct {
		in   int32
		want int
	}{
		{0, os.O_RDONLY},
		{processOpenRead, os.O_RDONLY},
		{processOpenWrite, os.O_WRONLY},
		{processOpenRead | processOpenWrite, os.O_RDWR},
		{processOpenWrite | processOpenAppend | processOpenCreate, os.O_WRONLY | os.O_APPEND | os.O_CREATE},
		{processOpenWrite | processOpenCreate | processOpenTruncate, os.O_WRONLY | os.O_CREATE | os.O_TRUNC},
		{processOpenWrite | processOpenCreate | processOpenExclusive, os.O_WRONLY | os.O_CREATE | os.O_EXCL},
	} {
		if got := processOpenFlags(tt.in); got != tt.want {
			t.Errorf("processOpenFlags(%#x) = %#x, want %#x", tt.in, got, tt.want)
		}
	}
}

func TestProcessWithBinDir(t *testing.T) {
	sep := string(filepath.ListSeparator)
	for _, tt := range []struct {
		name string
		env  []string
		dir  string
		want []string
	}{
		{"no dir", []string{"A=1"}, "", []string{"A=1"}},
		{"before PATH", []string{"A=1", "PATH=/x"}, "/bin/php", []string{"A=1", "PATH=/bin/php" + sep + "/x"}},
		{"no PATH", []string{"A=1"}, "/bin/php", []string{"A=1", "PATH=/bin/php" + sep + "/usr/local/bin:/usr/bin:/bin"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := processWithBinDir(slices.Clone(tt.env), tt.dir); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProcessResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix permissions")
	}
	cwd := t.TempDir()
	for _, f := range []struct {
		path string
		mode os.FileMode
	}{{"bin/prog", 0o755}, {"bin/plain", 0o644}, {"here", 0o755}} {
		name := filepath.Join(cwd, f.path)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name   string
		path   string
		search bool
		env    []string
		want   string // "" for not found
	}{
		{"absolute", "/bin/sh", true, nil, "/bin/sh"},
		{"relative with a slash", "bin/prog", true, nil, filepath.Join(cwd, "bin/prog")},
		{"relative without search", "prog", false, nil, filepath.Join(cwd, "prog")},
		{"relative PATH entry", "prog", true, []string{"PATH=bin"}, filepath.Join(cwd, "bin/prog")},
		{"empty PATH entry is the cwd", "here", true, []string{"PATH=/nonexistent:"}, filepath.Join(cwd, "here")},
		{"not executable", "plain", true, []string{"PATH=bin"}, ""},
		{"a directory", "bin", true, []string{"PATH=."}, ""},
		{"not found", "gophper-no-such-program", true, []string{"PATH=bin"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := processResolve(tt.path, tt.search, cwd, tt.env)
			if tt.want == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("got %q, %v, want not found", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestProcessWaitStatus(t *testing.T) {
	if got := processWaitStatus(nil); got != 127<<8 {
		t.Errorf("no state: %#x, want 127<<8", got)
	}
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	_ = cmd.Run()
	if got := processWaitStatus(cmd.ProcessState); got != 3<<8 {
		t.Errorf("exit 3: %#x, want 3<<8", got)
	}
}

func TestProcessGuardedReader(t *testing.T) {
	if (&processGuardedReader{}).reader() != nil {
		t.Error("no stdin: a child got a reader")
	}
	if f := os.Stdin; (&processGuardedReader{r: f}).reader() != io.Reader(f) {
		t.Error("a file stdin is not given as is")
	}
	g := &processGuardedReader{r: strings.NewReader("abc")}
	r := g.reader()
	buf := make([]byte, 2)
	if n, err := r.Read(buf); err != nil || string(buf[:n]) != "ab" {
		t.Errorf("read %q, %v", buf[:n], err)
	}
	g.close()
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Errorf("after close: %d, %v, want EOF", n, err)
	}
}

func TestProcessGuardedWriter(t *testing.T) {
	if (&processGuardedWriter{}).writer() != nil {
		t.Error("no stdout: a child got a writer")
	}
	if f := os.Stdout; (&processGuardedWriter{w: f}).writer() != io.Writer(f) {
		t.Error("a file stdout is not given as is")
	}
	var b bytes.Buffer
	g := &processGuardedWriter{w: &b}
	w := g.writer()
	if n, err := w.Write([]byte("kept")); n != 4 || err != nil {
		t.Errorf("write: %d, %v", n, err)
	}
	g.close()
	if n, err := w.Write([]byte("dropped")); n != 7 || err != nil {
		t.Errorf("write after close: %d, %v, want it to look written", n, err)
	}
	if b.String() != "kept" {
		t.Errorf("got %q, want only what came before Close", b.String())
	}
}
