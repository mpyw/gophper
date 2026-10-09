//declscope:namespace engine

package gophper

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper/internal/hostproc"
	"github.com/mpyw/gophper/internal/wasi"
)

// Signals for PHP. gophper-wasm's compat/gophper_signal.c keeps the
// handlers. The host marks a signal pending and interrupts the VM, and
// the guest raises it at the interrupt check, so pcntl handlers run.

// engineSignals is the signal state of one instance.
type engineSignals struct {
	mu sync.Mutex
	// handled and ignored are what the script set up, as bit sets: bit n
	// is signal n, numbered as on Linux.
	handled, ignored uint64
	pending          uint64
	// changed is closed when a signal becomes pending, then replaced.
	changed chan struct{}
	alarm   *time.Timer
	alarmAt time.Time
	// terminated is a fatal signal the guest did not act on in time.
	terminated int32
}

// Signals whose default action is to do nothing.
const engineSignalsIgnoredByDefault = 1<<17 | 1<<18 | 1<<23 | 1<<28 // CHLD, CONT, URG, WINCH

const engineSignalAlarm = 14

// engineSignalGrace is how long a guest gets to act on a fatal signal.
const engineSignalGrace = 5 * time.Second

// engineExportSignals adds the host functions to b.
func engineExportSignals(b wazero.HostModuleBuilder) {
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, handled, ignored int64) {
		if inst := engineInstanceFrom(ctx); inst != nil {
			inst.signals.mu.Lock()
			inst.signals.handled, inst.signals.ignored = uint64(handled), uint64(ignored)
			inst.signals.mu.Unlock()
		}
	}).Export("sig_watch")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context) int64 {
		inst := engineInstanceFrom(ctx)
		if inst == nil {
			return 0
		}
		inst.signals.mu.Lock()
		defer inst.signals.mu.Unlock()
		p := inst.signals.pending
		inst.signals.pending = 0
		return int64(p)
	}).Export("sig_take")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, mask int64, timeoutMs int32) int32 {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return inst.waitSignal(uint64(mask), timeoutMs)
		}
		return 0
	}).Export("sig_wait")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, seconds int32) int32 {
		if inst := engineInstanceFrom(ctx); inst != nil {
			return inst.setAlarm(seconds)
		}
		return 0
	}).Export("alarm")
}

// signal delivers sig, numbered as on Linux. The guest runs the script's
// handler, or the default action, which ends the run quietly. A guest that
// does not reach an interrupt check in time is stopped from here.
func (i *engineInstance) signal(sig int32) {
	if sig <= 0 || sig >= 64 {
		return
	}
	bit := uint64(1) << sig
	s := &i.signals
	s.mu.Lock()
	if s.ignored&bit != 0 || (s.handled&bit == 0 && engineSignalsIgnoredByDefault&bit != 0) {
		s.mu.Unlock()
		return
	}
	s.pending |= bit
	close(s.changed)
	s.changed = make(chan struct{})
	fatal := s.handled&bit == 0
	s.mu.Unlock()
	i.wake()
	if fatal {
		time.AfterFunc(engineSignalGrace, func() {
			s.mu.Lock()
			if s.terminated == 0 {
				s.terminated = sig
			}
			s.mu.Unlock()
			i.cancel()
		})
	}
}

// waitSignal blocks until a signal in mask is pending. It returns 1, 0 on
// timeout (negative: none), or -errno for an interrupt or the end of the run.
func (i *engineInstance) waitSignal(mask uint64, timeoutMs int32) int32 {
	var timer <-chan time.Time
	if timeoutMs >= 0 {
		t := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
		defer t.Stop()
		timer = t.C
	}
	intr := i.Interruption()
	for {
		i.signals.mu.Lock()
		ready := i.signals.pending&mask != 0
		changed := i.signals.changed
		i.signals.mu.Unlock()
		if ready {
			return 1
		}
		select {
		case <-changed:
		case <-timer:
			return 0
		case <-intr:
			return -wasi.EINTR
		case <-i.ctx.Done():
			return -wasi.EIO
		}
	}
}

// setAlarm replaces the alarm, as alarm(2) does, and returns the seconds
// the old one had left.
func (i *engineInstance) setAlarm(seconds int32) int32 {
	s := &i.signals
	s.mu.Lock()
	defer s.mu.Unlock()
	left := int32(0)
	if s.alarm != nil && s.alarm.Stop() {
		left = int32(max(time.Until(s.alarmAt).Round(time.Second)/time.Second, 1))
	}
	s.alarm = nil
	if seconds > 0 {
		d := time.Duration(seconds) * time.Second
		s.alarmAt = time.Now().Add(d)
		s.alarm = time.AfterFunc(d, func() { i.signal(engineSignalAlarm) })
	}
	return left
}

// forwardSignals delivers what arrives on ch until done is closed.
func (i *engineInstance) forwardSignals(ch <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case sig := <-ch:
			if n, ok := hostproc.ProcessSignalNumber(sig); ok {
				i.signal(n)
			}
		case <-done:
			return
		}
	}
}
