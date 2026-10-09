// Package hostsys answers the guest's questions about the system it runs
// on: users and groups, file locks and the host name. gophper-wasm's
// compat/gophper_sys.c is the guest side.
package hostsys

import (
	"context"
	"os"
	"sync"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero"
)

// ExportSystem adds the host functions to b, the "gophper" host module.
// from returns the state of the instance a call comes from.
func ExportSystem(b wazero.HostModuleBuilder, from func(context.Context) *System) {
	b.NewFunctionBuilder().WithFunc(userIDs).Export("user_ids")
	b.NewFunctionBuilder().WithFunc(userGet).Export("user_get")
	b.NewFunctionBuilder().WithFunc(userHostName).Export("host_name")
	x := systemLockExports{from: from}
	b.NewFunctionBuilder().WithFunc(x.lock).Export("file_lock")
	b.NewFunctionBuilder().WithFunc(x.unlock).Export("file_unlock")
	p := systemPathExports{from: from}
	b.NewFunctionBuilder().WithFunc(p.stat).Export("path_stat")
	b.NewFunctionBuilder().WithFunc(p.access).Export("path_access")
	b.NewFunctionBuilder().WithFunc(p.chmod).Export("path_chmod")
	b.NewFunctionBuilder().WithFunc(p.chown).Export("path_chown")
}

// SystemHostPath maps a path inside PHP to the host file behind it, and
// tells whether PHP may change it. See gophper.Options.HostPath.
type SystemHostPath func(path string) (host string, writable, ok bool)

// System is the state of one PHP instance: the files it holds locks on.
type System struct {
	run      wasi.Run
	hostPath SystemHostPath
	mu       sync.Mutex
	locks    map[int32]*os.File
}

// NewSystem returns the state for one PHP instance.
func NewSystem(run wasi.Run, hostPath SystemHostPath) *System {
	return &System{run: run, hostPath: hostPath, locks: map[int32]*os.File{}}
}

// Close releases every lock the script still holds.
func (s *System) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for fd, f := range s.locks {
		f.Close()
		delete(s.locks, fd)
	}
}
