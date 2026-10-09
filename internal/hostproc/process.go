// Package hostproc runs child processes for PHP. gophper-wasm's
// compat/gophper_proc.c implements posix_spawn, waitpid and kill with the
// host functions here, so proc_open, exec and the rest work.
//
// A child is a host process, started with os/exec. Its fds are what the
// guest described: host pipes and sockets, the instance's own stdio, or
// files opened again by path.
package hostproc

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/mpyw/gophper/internal/hostnet"
)

// What a child fd stands for, and the open flags of a file. Keep in sync
// with gophper-wasm (ABI.md).
const (
	processChildHost  int32 = 1
	processChildStdio int32 = 2
	processChildFile  int32 = 3

	processOpenRead      int32 = 1
	processOpenWrite     int32 = 2
	processOpenAppend    int32 = 4
	processOpenCreate    int32 = 8
	processOpenTruncate  int32 = 16
	processOpenExclusive int32 = 32
)

// processChildFDSize is the size of one struct gophper_child_fd.
const processChildFDSize = 24

// ExportProcesses adds the process host functions to b, the "gophper" host module.
// from returns the processes of the instance a call comes from.
func ExportProcesses(b wazero.HostModuleBuilder, from func(context.Context) *Processes) {
	x := processExports{from: from}
	b.NewFunctionBuilder().WithFunc(x.spawn).Export("proc_spawn")
	b.NewFunctionBuilder().WithFunc(x.wait).Export("proc_wait")
	b.NewFunctionBuilder().WithFunc(x.kill).Export("proc_kill")
}

// Processes holds the children of one PHP instance.
type Processes struct {
	run     hostnet.Run
	sockets *hostnet.Sockets
	stdin   *processGuardedReader
	stdout  *processGuardedWriter
	stderr  *processGuardedWriter

	mu       sync.Mutex
	children map[int32]*processChild
	// changed is closed when any child exits, then replaced.
	changed chan struct{}
}

// NewProcesses returns an empty table for one PHP instance. stdin, stdout
// and stderr are the instance's own. A child given one of them gets it until
// Close, even when it outlives the instance.
func NewProcesses(run hostnet.Run, sockets *hostnet.Sockets, stdin io.Reader, stdout, stderr io.Writer) *Processes {
	return &Processes{
		run:      run,
		sockets:  sockets,
		stdin:    &processGuardedReader{r: stdin},
		stdout:   &processGuardedWriter{w: stdout},
		stderr:   &processGuardedWriter{w: stderr},
		children: map[int32]*processChild{},
		changed:  make(chan struct{}),
	}
}

// Close detaches the children from the instance. They keep running, as
// children of an exited PHP would, and are reaped when they exit. From now
// on they read EOF from the instance's stdin, and their writes to its
// stdout and stderr are dropped.
func (p *Processes) Close() {
	p.stdin.close()
	p.stdout.close()
	p.stderr.close()
}

type processChild struct {
	cmd  *exec.Cmd
	done bool
	// status is a wait(2) status, as Linux and wasi-libc encode it.
	status int32
}

type processExports struct {
	from func(context.Context) *Processes
}

func (x processExports) spawn(ctx context.Context, m api.Module,
	pathPtr, pathLen uint32, search int32, argvPtr, argvLen, envpPtr, envpLen, cwdPtr, cwdLen uint32,
	fdsPtr uint32, nfds int32, pidPtr uint32,
) int32 {
	p := x.from(ctx)
	mem := m.Memory()
	read := func(ptr, n uint32) string {
		b, _ := mem.Read(ptr, n)
		return string(b)
	}
	path := read(pathPtr, pathLen)
	argv := processSplitNUL(read(argvPtr, argvLen))
	env := processSplitNUL(read(envpPtr, envpLen))
	cwd := read(cwdPtr, cwdLen)

	name, err := processResolve(path, search != 0, cwd, env)
	if err != nil {
		return errnoFrom(err)
	}
	cmd := &exec.Cmd{Path: name, Args: argv, Env: env, Dir: cwd}
	if len(cmd.Args) == 0 {
		cmd.Args = []string{path}
	}

	var release []func()
	defer func() {
		for _, r := range release {
			r()
		}
	}()
	for i := range nfds {
		base := fdsPtr + uint32(i)*processChildFDSize
		fd, kind, value, flags := processReadI32(mem, base), processReadI32(mem, base+4), processReadI32(mem, base+8), processReadI32(mem, base+12)
		fpath := read(uint32(processReadI32(mem, base+16)), uint32(processReadI32(mem, base+20)))

		var r io.Reader
		var w io.Writer
		var f *os.File
		switch kind {
		case processChildHost:
			hf, done, err := p.sockets.ChildFile(value)
			if err != nil {
				return errnoEBADF
			}
			release = append(release, done)
			f = hf
		case processChildStdio:
			switch value {
			case 0:
				r = p.stdin.reader()
			case 1:
				w = p.stdout.writer()
			case 2:
				w = p.stderr.writer()
			default:
				return errnoEBADF
			}
		case processChildFile:
			of, err := os.OpenFile(fpath, processOpenFlags(flags), 0o666)
			if err != nil {
				return errnoFrom(err)
			}
			release = append(release, func() { of.Close() })
			f = of
		default:
			return errnoEINVAL
		}
		if f != nil {
			r, w = f, f
		}

		switch fd {
		case 0:
			cmd.Stdin = r
		case 1:
			cmd.Stdout = w
		case 2:
			cmd.Stderr = w
		default:
			if f == nil {
				// os/exec passes only files above stderr.
				return errnoEBADF
			}
			for len(cmd.ExtraFiles) < int(fd)-2 {
				cmd.ExtraFiles = append(cmd.ExtraFiles, nil)
			}
			cmd.ExtraFiles[fd-3] = f
		}
	}

	if err := cmd.Start(); err != nil {
		return errnoFrom(err)
	}
	pid := int32(cmd.Process.Pid)
	c := &processChild{cmd: cmd}
	p.mu.Lock()
	p.children[pid] = c
	p.mu.Unlock()
	go func() {
		cmd.Wait()
		p.mu.Lock()
		defer p.mu.Unlock()
		c.done = true
		c.status = processWaitStatus(cmd.ProcessState)
		close(p.changed)
		p.changed = make(chan struct{})
	}()
	mem.WriteUint32Le(pidPtr, uint32(pid))
	return 0
}

// wait reaps pid, or any child for -1. It returns the pid, 0 when nohang
// is set and no child has exited, or -errno.
func (x processExports) wait(ctx context.Context, m api.Module, pid, nohang int32, statusPtr uint32) int32 {
	p := x.from(ctx)
	intr := p.run.Interruption()
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		found := false
		for cpid, c := range p.children {
			if pid != -1 && cpid != pid {
				continue
			}
			found = true
			if c.done {
				delete(p.children, cpid)
				m.Memory().WriteUint32Le(statusPtr, uint32(c.status))
				return cpid
			}
		}
		switch {
		case !found:
			return -errnoECHILD
		case nohang != 0:
			return 0
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
			p.mu.Lock()
		case <-intr:
			p.mu.Lock()
			return -errnoEINTR
		case <-p.run.Context().Done():
			// As with sockets: EINTR would make PHP retry forever.
			p.mu.Lock()
			return -errnoEIO
		}
	}
}

// kill signals a child. Other processes are out of reach: PHP sees only its
// own children.
func (x processExports) kill(ctx context.Context, pid, sig int32) int32 {
	p := x.from(ctx)
	p.mu.Lock()
	c := p.children[pid]
	p.mu.Unlock()
	if c == nil {
		return errnoESRCH
	}
	if sig == 0 {
		return 0
	}
	s, ok := processHostSignal(sig)
	if !ok {
		return errnoEINVAL
	}
	if err := c.cmd.Process.Signal(s); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return 0
		}
		return errnoFrom(err)
	}
	return 0
}

// resolve finds the program, as execve or execvp would.
func processResolve(path string, search bool, cwd string, env []string) (string, error) {
	if strings.Contains(path, "/") || !search {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		return path, nil
	}
	dirs := os.Getenv("PATH")
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			dirs = v
		}
	}
	for _, dir := range filepath.SplitList(dirs) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		name := filepath.Join(dir, path)
		if st, err := os.Stat(name); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return name, nil
		}
	}
	return "", fs.ErrNotExist
}

func processOpenFlags(f int32) int {
	var flags int
	switch {
	case f&processOpenRead != 0 && f&processOpenWrite != 0:
		flags = os.O_RDWR
	case f&processOpenWrite != 0:
		flags = os.O_WRONLY
	default:
		flags = os.O_RDONLY
	}
	if f&processOpenAppend != 0 {
		flags |= os.O_APPEND
	}
	if f&processOpenCreate != 0 {
		flags |= os.O_CREATE
	}
	if f&processOpenTruncate != 0 {
		flags |= os.O_TRUNC
	}
	if f&processOpenExclusive != 0 {
		flags |= os.O_EXCL
	}
	return flags
}

func processSplitNUL(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\x00"), "\x00")
}

func processReadI32(mem api.Memory, ptr uint32) int32 {
	v, _ := mem.ReadUint32Le(ptr)
	return int32(v)
}

// processWaitStatus encodes how a child ended, as wait(2) does on Linux.
func processWaitStatus(ps *os.ProcessState) int32 {
	if ps == nil {
		// It could not be waited for. Report a failure, as a shell would.
		return 127 << 8
	}
	if sig, ok := processExitSignal(ps); ok {
		return sig & 0x7f
	}
	return int32(ps.ExitCode()&0xff) << 8
}

// processGuardedReader is the instance's stdin as children see it.
type processGuardedReader struct {
	mu     sync.Mutex
	r      io.Reader
	closed bool
}

// reader returns what a child reads: the stdin itself if it is a file, so
// that the child shares it as with a real fd.
func (g *processGuardedReader) reader() io.Reader {
	if f, ok := g.r.(*os.File); ok {
		return f
	}
	if g.r == nil {
		return nil
	}
	return g
}

func (g *processGuardedReader) Read(p []byte) (int, error) {
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	return g.r.Read(p)
}

func (g *processGuardedReader) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// processGuardedWriter is the instance's stdout or stderr as children see it. A
// child that outlives the instance must not write to, say, an HTTP
// response that is already finished.
type processGuardedWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (g *processGuardedWriter) writer() io.Writer {
	if f, ok := g.w.(*os.File); ok {
		return f
	}
	if g.w == nil {
		return nil
	}
	return g
}

func (g *processGuardedWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return len(p), nil
	}
	return g.w.Write(p)
}

func (g *processGuardedWriter) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}
