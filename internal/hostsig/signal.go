// Package hostsig gives PHP signals through host functions.
//
// gophper-wasm's compat/gophper_signal.c keeps the handlers. The host
// marks a signal pending and wakes the VM, and the guest raises it at the
// interrupt check, so pcntl handlers run. Signals are numbered as on Linux.
package hostsig

import (
	"context"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper/internal/wasi"
)

// Run is what signals need from the PHP instance: to wake its VM, as an
// interrupt does, and to end it.
type Run interface {
	wasi.Run
	Wake()
	Cancel()
}

// Signals is the signal state of one instance.
type Signals struct {
	run Run

	mu sync.Mutex
	// handled and ignored are what the script set up, as bit sets: bit n
	// is signal n.
	handled, ignored uint64
	pending          uint64
	// fresh is set by Deliver, and cleared once a sleep returned for it or
	// the guest took the signals. A handler runs with the next signal still
	// pending in the host, and each of its sleeps would return at once.
	fresh bool
	// changed is closed when a signal becomes pending, then replaced.
	changed chan struct{}
	alarm   *time.Timer
	alarmAt time.Time
	// grace are the timers that stop a guest slow to act on a fatal signal.
	grace []*time.Timer
	// stopped keeps timers from starting after the run.
	stopped bool
	// terminated is a fatal signal the guest did not act on in time.
	terminated int32
}

// Signals whose default action is to do nothing.
const signalIgnoredByDefault = 1<<17 | 1<<18 | 1<<23 | 1<<28 // CHLD, CONT, URG, WINCH

const signalAlarm = 14

// signalGrace is how long a guest gets to act on a fatal signal. A
// variable, so that tests need not wait as long.
var signalGrace = 5 * time.Second

// NewSignals returns the state for one instance.
func NewSignals(run Run) *Signals {
	return &Signals{run: run, changed: make(chan struct{})}
}

// ExportSignals adds the host functions to b. from returns the state of
// the instance a call comes from, or nil.
func ExportSignals(b wazero.HostModuleBuilder, from func(context.Context) *Signals) {
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, handled, ignored int64) {
		if s := from(ctx); s != nil {
			s.mu.Lock()
			s.handled, s.ignored = uint64(handled), uint64(ignored)
			s.mu.Unlock()
		}
	}).Export("sig_watch")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context) int64 {
		s := from(ctx)
		if s == nil {
			return 0
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.pending
		s.pending = 0
		s.fresh = false
		return int64(p)
	}).Export("sig_take")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, mask int64, timeoutMs int32) int32 {
		if s := from(ctx); s != nil {
			return s.wait(uint64(mask), timeoutMs)
		}
		return 0
	}).Export("sig_wait")
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, seconds int32) int32 {
		if s := from(ctx); s != nil {
			return s.SetAlarm(seconds)
		}
		return 0
	}).Export("alarm")
}

// Deliver delivers sig. The guest runs the script's handler, or the
// default action, which ends the run quietly. A guest that does not reach
// an interrupt check in time is stopped from here.
func (s *Signals) Deliver(sig int32) {
	if sig <= 0 || sig >= 64 {
		return
	}
	bit := uint64(1) << sig
	s.mu.Lock()
	if s.ignored&bit != 0 || (s.handled&bit == 0 && signalIgnoredByDefault&bit != 0) {
		s.mu.Unlock()
		return
	}
	s.pending |= bit
	s.fresh = true
	close(s.changed)
	s.changed = make(chan struct{})
	fatal := s.handled&bit == 0
	s.mu.Unlock()
	s.run.Wake()
	if !fatal {
		return
	}
	t := time.AfterFunc(signalGrace, func() {
		s.mu.Lock()
		if s.terminated == 0 {
			s.terminated = sig
		}
		s.mu.Unlock()
		s.run.Cancel()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		t.Stop()
		return
	}
	s.grace = append(s.grace, t)
}

// Stop cancels the alarm and the grace timers, at the end of the run.
func (s *Signals) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.alarm != nil {
		s.alarm.Stop()
		s.alarm = nil
	}
	for _, t := range s.grace {
		t.Stop()
	}
	s.grace = nil
}

// Pending reports, once for each delivery, whether a signal waits for the
// guest. A sleep returns early for it once. Native PHP blocks signals while
// a handler runs, so a handler's sleeps sleep in full, as here.
func (s *Signals) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := s.fresh && s.pending != 0
	s.fresh = false
	return fresh
}

// Terminated is the fatal signal that ended the run from the host, or 0.
func (s *Signals) Terminated() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminated
}

// wait blocks until a signal in mask is pending. It returns 1, 0 on
// timeout (negative: none), or -errno for an interrupt or the end of the run.
func (s *Signals) wait(mask uint64, timeoutMs int32) int32 {
	var timer <-chan time.Time
	if timeoutMs >= 0 {
		t := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
		defer t.Stop()
		timer = t.C
	}
	intr := s.run.Interruption()
	for {
		s.mu.Lock()
		ready := s.pending&mask != 0
		changed := s.changed
		s.mu.Unlock()
		if ready {
			return 1
		}
		select {
		case <-changed:
		case <-timer:
			return 0
		case <-intr:
			return -wasi.EINTR
		case <-s.run.Context().Done():
			return -wasi.EIO
		}
	}
}

// SetAlarm replaces the alarm, as alarm(2) does, and returns the seconds
// the old one had left. Zero cancels it.
func (s *Signals) SetAlarm(seconds int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := int32(0)
	if s.alarm != nil && s.alarm.Stop() {
		left = int32(max(time.Until(s.alarmAt).Round(time.Second)/time.Second, 1))
	}
	s.alarm = nil
	if seconds > 0 && !s.stopped {
		d := time.Duration(seconds) * time.Second
		s.alarmAt = time.Now().Add(d)
		s.alarm = time.AfterFunc(d, func() { s.Deliver(signalAlarm) })
	}
	return left
}
