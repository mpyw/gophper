// Package hostvm is the host side of one PHP instance's virtual machine:
// its linear memory, the interrupt flags php-src checks, and the timer of
// max_execution_time.
//
// php-src arms that timer with setitimer(2), and its signal handler sets
// EG(timed_out) and EG(vm_interrupt). The VM then raises "Maximum execution
// time exceeded" at its next interrupt check. WASI has neither.
// gophper-wasm's patches/0004 calls the host's set_timeout instead, and the
// timer writes the same flags into linear memory.
//
// The timer writes from its own goroutine, while the module may be growing
// its memory. So VM allocates the linear memory itself, and both go through
// one mutex.
//
// A script blocked inside an internal function, other than sleep and
// sockets, stops only when that function returns.
package hostvm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// VM is one instance's machine. It implements wasi.Run, and
// experimental.MemoryAllocator for the module it runs.
type VM struct {
	ctx    context.Context
	cancel context.CancelFunc
	memory memory

	// Pending reports whether a signal waits for the guest. Nanosleep
	// returns at once then. Nil means none ever does.
	Pending func() bool

	// Addresses of EG(vm_interrupt), EG(timed_out) and EG(hard_timeout).
	vmInterrupt, timedOut, hardTimeout uint32

	mu    sync.Mutex
	timer *time.Timer
	// interrupted is closed by the next interrupt, then replaced.
	interrupted chan struct{}
}

// NewVM returns the machine for one run. cancel ends it.
func NewVM(ctx context.Context, cancel context.CancelFunc) *VM {
	return &VM{ctx: ctx, cancel: cancel, interrupted: make(chan struct{})}
}

// ExportVM adds set_timeout to b. from returns the VM a call comes from, or
// nil.
func ExportVM(b wazero.HostModuleBuilder, from func(context.Context) *VM) {
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, seconds int32) {
		if v := from(ctx); v != nil {
			v.SetTimeout(seconds)
		}
	}).Export("set_timeout")
}

// Allocate implements experimental.MemoryAllocator.
func (v *VM) Allocate(capacity, max uint64) experimental.LinearMemory {
	v.memory.buf = make([]byte, 0, capacity)
	v.memory.max = max
	return &v.memory
}

// Locate finds the interrupt flags in m. gophper-wasm's ABI.md describes
// the exports.
func (v *VM) Locate(m api.Module) error {
	addr := func(name string) (uint32, error) {
		g := m.ExportedGlobal(name)
		if g == nil {
			return 0, fmt.Errorf("php.wasm does not export %s", name)
		}
		// The global holds the address of a pointer, which points to the value.
		p, ok := m.Memory().ReadUint32Le(uint32(g.Get()))
		if !ok {
			return 0, fmt.Errorf("%s points outside memory", name)
		}
		return p, nil
	}
	var err error
	if v.vmInterrupt, err = addr("gophper_vm_interrupt"); err != nil {
		return err
	}
	if v.timedOut, err = addr("gophper_timed_out"); err != nil {
		return err
	}
	v.hardTimeout, err = addr("gophper_hard_timeout")
	return err
}

// Context implements wasi.Run.
func (v *VM) Context() context.Context { return v.ctx }

// Interruption implements wasi.Run.
func (v *VM) Interruption() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.interrupted
}

// Cancel ends the run.
func (v *VM) Cancel() { v.cancel() }

// SetTimeout replaces the timer. Zero cancels it.
func (v *VM) SetTimeout(seconds int32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.timer != nil {
		v.timer.Stop()
		v.timer = nil
	}
	if seconds > 0 {
		v.timer = time.AfterFunc(time.Duration(seconds)*time.Second, v.fire)
	}
}

// fire does what zend_timeout_handler() does for a non-ZTS build.
func (v *VM) fire() {
	if v.memory.readByte(v.timedOut) != 0 {
		// The script has not reached an interrupt check since the last
		// timeout. php-src exits with 124 here. A host cannot stop wasm code
		// from outside, so the flags are raised once more and kept.
		v.Interrupt()
		return
	}
	v.Interrupt()
	if hard := v.memory.readUint32(v.hardTimeout); hard > 0 {
		v.SetTimeout(int32(hard))
	}
}

// Interrupt makes the VM raise "Maximum execution time exceeded" at its
// next check.
func (v *VM) Interrupt() {
	v.memory.writeByte(v.timedOut, 1)
	v.Wake()
}

// Wake makes the VM call zend_interrupt_function at its next check, and
// cuts blocking host calls short, as a signal would.
func (v *VM) Wake() {
	v.memory.writeByte(v.vmInterrupt, 1)
	v.mu.Lock()
	close(v.interrupted)
	v.interrupted = make(chan struct{})
	v.mu.Unlock()
}

// InterruptUntil raises interrupts until done is closed. One interrupt can
// be lost: php_request_startup() clears the flags, so a cancel that arrives
// before it would leave a script running forever.
func (v *VM) InterruptUntil(done <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		v.Interrupt()
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

// Nanosleep sleeps like nanosleep(2), but returns early on an interrupt or
// when the run's context is done. Without it, usleep() in PHP blocks inside
// the host call, and a timeout waits for the sleep to end.
func (v *VM) Nanosleep(ns int64) {
	// Taken before sleeping, so an earlier interrupt does not cut this one short.
	intr := v.Interruption()
	// A signal that arrived just before is not in intr. The guest takes it
	// at its next interrupt check, so sleeping now would delay its handler.
	if v.Pending != nil && v.Pending() {
		return
	}
	t := time.NewTimer(time.Duration(ns))
	defer t.Stop()
	select {
	case <-t.C:
	case <-intr:
	case <-v.ctx.Done():
	}
}

// memory is a linear memory that the timer goroutine can write to.
// Reallocate runs on the module's goroutine. The flag accessors run on the
// timer's. mu orders them, so a flag never lands in a buffer already
// replaced.
type memory struct {
	mu  sync.Mutex
	buf []byte
	max uint64
}

// Reallocate implements experimental.LinearMemory.
func (m *memory) Reallocate(size uint64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > m.max {
		return nil
	}
	if size <= uint64(cap(m.buf)) {
		// Bytes past the old length were never handed out, so they are still zero.
		m.buf = m.buf[:size]
		return m.buf
	}
	grown := min(max(size, uint64(cap(m.buf))*2), m.max)
	buf := make([]byte, size, grown)
	copy(buf, m.buf)
	m.buf = buf
	return buf
}

// Free implements experimental.LinearMemory.
func (m *memory) Free() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = nil
}

func (m *memory) readByte(addr uint32) byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) >= len(m.buf) {
		return 0
	}
	return m.buf[addr]
}

func (m *memory) readUint32(addr uint32) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr)+4 > len(m.buf) {
		return 0
	}
	b := m.buf[addr : addr+4]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func (m *memory) writeByte(addr uint32, v byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) < len(m.buf) {
		m.buf[addr] = v
	}
}
