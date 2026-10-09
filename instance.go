//declscope:namespace engine

package gophper

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/mpyw/gophper/internal/hostnet"
)

// engineInstance is the host side of one running PHP instance.
//
// It runs max_execution_time for PHP. php-src arms a timer with setitimer(2),
// and its signal handler sets EG(timed_out) and EG(vm_interrupt). The VM
// then raises "Maximum execution time exceeded" at its next interrupt check.
// WASI has neither. gophper-wasm's patches/0004 calls engineSetTimeout
// instead, and fire writes the same flags into linear memory.
//
// The timer writes from its own goroutine, while the module may be growing
// its memory. So the instance allocates the linear memory itself
// (engineMemory), and both go through one mutex.
//
// A script blocked inside an internal function, other than sleep and
// sockets, stops only when that function returns.
type engineInstance struct {
	ctx     context.Context
	sockets *hostnet.Sockets
	memory  engineMemory

	// Addresses of EG(vm_interrupt), EG(timed_out) and EG(hard_timeout).
	vmInterrupt, timedOut, hardTimeout uint32

	mu    sync.Mutex
	timer *time.Timer
	// interrupted is closed by the next interrupt, then replaced.
	interrupted chan struct{}
}

func newEngineInstance(ctx context.Context) *engineInstance {
	inst := &engineInstance{ctx: ctx, interrupted: make(chan struct{})}
	inst.sockets = hostnet.NewSockets(inst)
	return inst
}

type engineInstanceKey struct{}

// engineInstanceFrom returns the instance a host call comes from.
func engineInstanceFrom(ctx context.Context) *engineInstance {
	inst, _ := ctx.Value(engineInstanceKey{}).(*engineInstance)
	return inst
}

// Allocate implements experimental.MemoryAllocator.
func (i *engineInstance) Allocate(capacity, max uint64) experimental.LinearMemory {
	i.memory.buf = make([]byte, 0, capacity)
	i.memory.max = max
	return &i.memory
}

// locate finds the interrupt flags. gophper-wasm's ABI.md describes the exports.
func (i *engineInstance) locate(m api.Module) error {
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
	if i.vmInterrupt, err = addr("gophper_vm_interrupt"); err != nil {
		return err
	}
	if i.timedOut, err = addr("gophper_timed_out"); err != nil {
		return err
	}
	i.hardTimeout, err = addr("gophper_hard_timeout")
	return err
}

// Context implements hostnet.Run.
func (i *engineInstance) Context() context.Context { return i.ctx }

// Interruption implements hostnet.Run.
func (i *engineInstance) Interruption() <-chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.interrupted
}

// engineSetTimeout is imported by php-src as gophper.set_timeout.
func engineSetTimeout(ctx context.Context, seconds int32) {
	if inst := engineInstanceFrom(ctx); inst != nil {
		inst.setTimeout(seconds)
	}
}

// setTimeout replaces the timer. Zero cancels it.
func (i *engineInstance) setTimeout(seconds int32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.timer != nil {
		i.timer.Stop()
		i.timer = nil
	}
	if seconds > 0 {
		i.timer = time.AfterFunc(time.Duration(seconds)*time.Second, i.fire)
	}
}

// fire does what zend_timeout_handler() does for a non-ZTS build.
func (i *engineInstance) fire() {
	if i.memory.readByte(i.timedOut) != 0 {
		// The script has not reached an interrupt check since the last
		// timeout. php-src exits with 124 here. A host cannot stop wasm code
		// from outside, so the flags are raised once more and kept.
		i.interrupt()
		return
	}
	i.interrupt()
	if hard := i.memory.readUint32(i.hardTimeout); hard > 0 {
		i.setTimeout(int32(hard))
	}
}

// interrupt makes the VM raise "Maximum execution time exceeded" at its next check.
func (i *engineInstance) interrupt() {
	i.memory.writeByte(i.timedOut, 1)
	i.memory.writeByte(i.vmInterrupt, 1)
	i.mu.Lock()
	close(i.interrupted)
	i.interrupted = make(chan struct{})
	i.mu.Unlock()
}

// nanosleep sleeps like nanosleep(2), but returns early on an interrupt or
// when the run's context is done. Without it, usleep() in PHP blocks inside
// the host call, and a timeout waits for the sleep to end.
func (i *engineInstance) nanosleep(ns int64) {
	// Taken before sleeping, so an earlier interrupt does not cut this one short.
	intr := i.Interruption()
	t := time.NewTimer(time.Duration(ns))
	defer t.Stop()
	select {
	case <-t.C:
	case <-intr:
	case <-i.ctx.Done():
	}
}

// engineMemory is a linear memory that the timer goroutine can write to.
// Reallocate runs on the module's goroutine. The flag accessors run on the
// timer's. mu orders them, so a flag never lands in a buffer already
// replaced.
type engineMemory struct {
	mu  sync.Mutex
	buf []byte
	max uint64
}

// Reallocate implements experimental.LinearMemory.
func (m *engineMemory) Reallocate(size uint64) []byte {
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
func (m *engineMemory) Free() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = nil
}

func (m *engineMemory) readByte(addr uint32) byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) >= len(m.buf) {
		return 0
	}
	return m.buf[addr]
}

func (m *engineMemory) readUint32(addr uint32) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr)+4 > len(m.buf) {
		return 0
	}
	b := m.buf[addr : addr+4]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func (m *engineMemory) writeByte(addr uint32, v byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(addr) < len(m.buf) {
		m.buf[addr] = v
	}
}
