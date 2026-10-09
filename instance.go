//declscope:namespace engine

package gophper

import (
	"context"
	"os"

	"github.com/mpyw/gophper/internal/dylink"
	"github.com/mpyw/gophper/internal/hostfn"
	"github.com/mpyw/gophper/internal/hostnet"
	"github.com/mpyw/gophper/internal/hostproc"
	"github.com/mpyw/gophper/internal/hostsig"
	"github.com/mpyw/gophper/internal/hostsys"
	"github.com/mpyw/gophper/internal/hostvm"
)

// engineInstance is the host side of one running PHP instance: the state
// of each package of host functions. The VM is the wasi.Run they share.
type engineInstance struct {
	vm        *hostvm.VM
	signals   *hostsig.Signals
	sockets   *hostnet.Sockets
	processes *hostproc.Processes
	system    *hostsys.System
	functions *hostfn.Functions
	linker    *dylink.Linker
}

func newEngineInstance(ctx context.Context, cancel context.CancelFunc, extensions *dylink.Cache, binDir string, opts Options) *engineInstance {
	vm := hostvm.NewVM(ctx, cancel)
	vm.MemoryLimit = uint64(opts.MemoryLimit)
	inst := &engineInstance{vm: vm, signals: hostsig.NewSignals(vm), linker: dylink.NewLinker(extensions)}
	vm.Pending = inst.signals.Pending
	hostPath := opts.HostPath
	if hostPath == nil {
		hostPath = func(string) (string, bool, bool) { return "", false, false }
	}
	inst.sockets = hostnet.NewSockets(vm, hostnet.SocketsHostPath(hostPath), opts.Network)
	inst.processes = hostproc.NewProcesses(vm, inst.sockets, opts.Processes, hostPath, binDir, opts.Stdin, opts.Stdout, opts.Stderr)
	inst.system = hostsys.NewSystem(vm, hostPath)
	fns := make(map[string]hostfn.Function, len(opts.Functions))
	for name, fn := range opts.Functions {
		fns[name] = hostfn.Function(fn)
	}
	inst.functions = hostfn.NewFunctions(ctx, fns)
	return inst
}

type engineInstanceKey struct{}

// engineInstanceFrom returns the instance a host call comes from.
func engineInstanceFrom(ctx context.Context) *engineInstance {
	inst, _ := ctx.Value(engineInstanceKey{}).(*engineInstance)
	return inst
}

// engineFrom returns a function that finds one part of the instance a host
// call comes from, or the zero value outside any.
func engineFrom[T any](part func(*engineInstance) T) func(context.Context) T {
	return func(ctx context.Context) T {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return part(inst)
		}
		var zero T
		return zero
	}
}

// forwardSignals delivers what arrives on ch until done is closed.
func (i *engineInstance) forwardSignals(ch <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case sig := <-ch:
			if n, ok := hostproc.ProcessSignalNumber(sig); ok {
				i.signals.Deliver(n)
			}
		case <-done:
			return
		}
	}
}
