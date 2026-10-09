package hostvm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// TestVMStop keeps the timer off after the run, even if it is set again,
// as a hard timeout firing at the end would.
func TestVMStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := NewVM(ctx, cancel)
	v.SetTimeout(60)
	if v.timer == nil {
		t.Fatal("no timer")
	}
	timer := v.timer
	v.Stop()
	if timer.Stop() {
		t.Error("the timer was still running")
	}
	v.SetTimeout(60)
	if v.timer != nil {
		t.Error("a timer started after Stop")
	}
}

// vmTestModule returns a module with one page of memory that exports an
// i32 global for each name, holding the address in values.
func vmTestModule(t *testing.T, names []string, values []int32) api.Module {
	t.Helper()
	section := func(id byte, body []byte) []byte {
		return append([]byte{id, byte(len(body))}, body...)
	}
	globals := []byte{byte(len(names))}
	exports := []byte{byte(len(names))}
	for i, name := range names {
		// i32, immutable, i32.const value (one LEB128 byte or two), end.
		v := values[i]
		globals = append(globals, 0x7f, 0x00, 0x41)
		for {
			b := byte(v & 0x7f)
			v >>= 7
			if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
				globals = append(globals, b)
				break
			}
			globals = append(globals, b|0x80)
		}
		globals = append(globals, 0x0b)
		exports = append(append(append(exports, byte(len(name))), name...), 0x03, byte(i))
	}
	bin := []byte("\x00asm\x01\x00\x00\x00")
	bin = append(bin, section(5, []byte{1, 0, 1})...)
	bin = append(bin, section(6, globals)...)
	bin = append(bin, section(7, exports)...)
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { _ = r.Close(ctx) })
	m, err := r.Instantiate(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVMLocate(t *testing.T) {
	all := []string{"gophper_vm_interrupt", "gophper_timed_out", "gophper_hard_timeout"}
	for n := range all {
		// Each export missing in turn.
		m := vmTestModule(t, all[:n], []int32{0, 4, 8}[:n])
		if err := NewVM(context.Background(), func() {}).Locate(m); err == nil || !strings.Contains(err.Error(), all[n]) {
			t.Errorf("without %s: %v", all[n], err)
		}
	}
	// A global outside memory.
	m := vmTestModule(t, all, []int32{0, 1 << 20, 8})
	if err := NewVM(context.Background(), func() {}).Locate(m); err == nil || !strings.Contains(err.Error(), "outside memory") {
		t.Errorf("outside memory: %v", err)
	}
	// Each global points at a pointer to the flag.
	m = vmTestModule(t, all, []int32{0, 4, 8})
	for i, p := range []uint32{100, 104, 108} {
		m.Memory().WriteUint32Le(uint32(4*i), p)
	}
	v := NewVM(context.Background(), func() {})
	if err := v.Locate(m); err != nil {
		t.Fatal(err)
	}
	if v.vmInterrupt != 100 || v.timedOut != 104 || v.hardTimeout != 108 {
		t.Errorf("located %d, %d, %d", v.vmInterrupt, v.timedOut, v.hardTimeout)
	}
}

// TestVMFire raises the flags, and once more if the script has not seen
// the last timeout. A hard timeout arms the timer again.
func TestVMFire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := NewVM(ctx, cancel)
	v.Allocate(64, 64)
	v.memory.Reallocate(64)
	v.vmInterrupt, v.timedOut, v.hardTimeout = 1, 2, 4
	v.memory.buf[4] = 30 // hard_timeout = 30
	intr := v.Interruption()
	v.fire()
	<-intr
	if v.memory.buf[1] != 1 || v.memory.buf[2] != 1 {
		t.Errorf("flags %v", v.memory.buf[:3])
	}
	v.mu.Lock()
	armed := v.timer != nil
	v.mu.Unlock()
	if !armed {
		t.Error("the hard timeout was not armed")
	}
	// Not seen yet: raised again, and no timer is armed after it.
	v.SetTimeout(0)
	v.memory.buf[1] = 0
	v.fire()
	if v.memory.buf[1] != 1 || v.timer != nil {
		t.Errorf("flags %v, timer %v", v.memory.buf[:3], v.timer)
	}
	v.Stop()
}

func TestVMNanosleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	v := NewVM(ctx, cancel)
	v.Allocate(8, 8)
	v.memory.Reallocate(8)
	v.vmInterrupt = 1
	// A pending signal returns at once, with the flag raised: Deliver may
	// not have raised it yet, and the guest checks right after.
	v.Pending = func() bool { return true }
	start := time.Now()
	v.Nanosleep(int64(time.Hour))
	if v.memory.buf[1] != 1 {
		t.Error("returned for a signal with no interrupt flag raised")
	}
	v.Pending = nil
	// So does the end of the run.
	v.Cancel()
	v.Nanosleep(int64(time.Hour))
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("slept %s", d)
	}
}

func TestVMMemory(t *testing.T) {
	var v VM
	mem := v.Allocate(0, 8)
	if b := mem.Reallocate(16); b != nil {
		t.Errorf("past the maximum: %d bytes", len(b))
	}
	if b := mem.Reallocate(4); len(b) != 4 {
		t.Fatalf("grown to %d bytes", len(b))
	}
	// Outside the memory: reads are zero, and writes are dropped.
	v.memory.writeByte(4, 1)
	if v.memory.readByte(4) != 0 || v.memory.readUint32(1) != 0 {
		t.Error("read outside memory")
	}
	v.memory.writeByte(0, 7)
	if b := mem.Reallocate(8); b[0] != 7 || len(b) != 8 {
		t.Errorf("grown to %v", b)
	}
	if v.memory.readUint32(0) != 7 {
		t.Errorf("read %d", v.memory.readUint32(0))
	}
	mem.Free()
	if v.memory.readByte(0) != 0 {
		t.Error("read after Free")
	}
}
