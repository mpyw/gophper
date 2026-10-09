package hostsig

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper/internal/wasi"
)

type testRun struct {
	ctx      context.Context
	intr     chan struct{}
	woken    atomic.Int32
	canceled atomic.Bool
}

func newTestRun(ctx context.Context) *testRun {
	return &testRun{ctx: ctx, intr: make(chan struct{})}
}

func (r *testRun) Context() context.Context      { return r.ctx }
func (r *testRun) Interruption() <-chan struct{} { return r.intr }
func (r *testRun) Wake()                         { r.woken.Add(1) }
func (r *testRun) Cancel()                       { r.canceled.Store(true) }

// TestSignalsStop cancels the timers a run left, so none fires on a later run.
func TestSignalsStop(t *testing.T) {
	s := NewSignals(newTestRun(context.Background()))
	s.Deliver(15) // SIGTERM, fatal without a handler
	if s.SetAlarm(60) != 0 {
		t.Error("an alarm was already set")
	}
	if len(s.grace) != 1 || s.alarm == nil {
		t.Fatalf("grace %d, alarm %v", len(s.grace), s.alarm)
	}
	grace, alarm := s.grace[15], s.alarm
	s.Stop()
	if grace.Stop() || alarm.Stop() {
		t.Error("a timer was still running")
	}
	s.Deliver(15)
	if s.SetAlarm(60) != 0 || len(s.grace) != 0 || s.alarm != nil {
		t.Errorf("a timer started after Stop: grace %d, alarm %v", len(s.grace), s.alarm)
	}
}

// TestSignalsDeliver marks what the script handles pending, and drops what
// it ignores. A fatal signal the guest does not act on in time ends the run.
func TestSignalsDeliver(t *testing.T) {
	signalGrace = 10 * time.Millisecond
	defer func() { signalGrace = 5 * time.Second }()
	run := newTestRun(context.Background())
	s := NewSignals(run)
	defer s.Stop()
	s.handled, s.ignored = 1<<10, 1<<12
	for _, sig := range []int32{0, 64, -1, 12, 17} {
		// Out of range, ignored, and ignored by default (SIGCHLD).
		s.Deliver(sig)
	}
	if s.Pending() || run.woken.Load() != 0 {
		t.Fatal("a dropped signal is pending")
	}
	s.Deliver(10)
	if !s.Pending() || run.woken.Load() != 1 {
		t.Fatal("SIGUSR1 is not pending")
	}
	if s.wait(1<<10, -1) != 1 {
		t.Error("wait did not see SIGUSR1")
	}
	s.Deliver(15)
	deadline := time.Now().Add(10 * time.Second)
	for !run.canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Terminated() != 15 || !run.canceled.Load() {
		t.Errorf("terminated %d, canceled %v", s.Terminated(), run.canceled.Load())
	}
}

func TestSignalsWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := newTestRun(ctx)
	s := NewSignals(run)
	if got := s.wait(1, 0); got != 0 {
		t.Errorf("timeout: %d", got)
	}
	// A signal while waiting.
	s.handled = 1 << 10
	go func() {
		time.Sleep(10 * time.Millisecond)
		s.Deliver(10)
	}()
	if got := s.wait(1<<10, 10_000); got != 1 {
		t.Errorf("signal: %d", got)
	}
	close(run.intr)
	if got := s.wait(1<<11, -1); got != -wasi.EINTR {
		t.Errorf("interrupt: %d", got)
	}
	run.intr = make(chan struct{})
	cancel()
	if got := s.wait(1<<11, -1); got != -wasi.EIO {
		t.Errorf("end of the run: %d", got)
	}
}

func TestSignalsAlarm(t *testing.T) {
	s := NewSignals(newTestRun(context.Background()))
	defer s.Stop()
	s.SetAlarm(30)
	if left := s.SetAlarm(0); left < 29 || left > 30 {
		t.Errorf("left %d", left)
	}
	if left := s.SetAlarm(0); left != 0 {
		t.Errorf("left %d after cancel", left)
	}
}

// TestExportSignals calls the host functions from outside an instance,
// where from finds none.
func TestExportSignals(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer func() { _ = r.Close(ctx) }()
	var s *Signals
	b := r.NewHostModuleBuilder("env")
	ExportSignals(b, func(context.Context) *Signals { return s })
	if _, err := b.Instantiate(ctx); err != nil {
		t.Fatal(err)
	}
	// Host modules cannot be called from Go, so a guest module re-exports
	// the four functions. i32 is 0x7f, i64 0x7e.
	sigs := []struct {
		name            string
		params, results string
	}{
		{"sig_watch", "\x7e\x7e", ""},
		{"sig_take", "", "\x7e"},
		{"sig_wait", "\x7e\x7f", "\x7f"},
		{"alarm", "\x7f", "\x7f"},
	}
	section := func(id byte, count int, body []byte) []byte {
		return append([]byte{id, byte(len(body) + 1), byte(count)}, body...)
	}
	var types, imports, exports []byte
	for i, f := range sigs {
		types = append(types, 0x60, byte(len(f.params)))
		types = append(append(types, f.params...), byte(len(f.results)))
		types = append(types, f.results...)
		imports = append(append(imports, 3), "env"...)
		imports = append(append(append(imports, byte(len(f.name))), f.name...), 0x00, byte(i))
		exports = append(append(append(exports, byte(len(f.name))), f.name...), 0x00, byte(i))
	}
	bin := []byte("\x00asm\x01\x00\x00\x00")
	bin = append(bin, section(1, len(sigs), types)...)
	bin = append(bin, section(2, len(sigs), imports)...)
	bin = append(bin, section(7, len(sigs), exports)...)
	m, err := r.Instantiate(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args ...uint64) uint64 {
		t.Helper()
		res, err := m.ExportedFunction(name).Call(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) == 0 {
			return 0
		}
		return res[0]
	}
	for _, inst := range []*Signals{nil, NewSignals(newTestRun(ctx))} {
		s = inst
		call("sig_watch", 1<<10, 0)
		if s != nil {
			s.Deliver(10)
		}
		want := uint64(0)
		if s != nil {
			want = 1 << 10
		}
		if got := call("sig_take"); got != want {
			t.Errorf("sig_take = %d, want %d", got, want)
		}
		if got := call("sig_wait", 1<<10, 0); got != 0 {
			t.Errorf("sig_wait = %d", got)
		}
		if got := call("alarm", 0); got != 0 {
			t.Errorf("alarm = %d", got)
		}
	}
}

// TestSignalsTakenNotFatal takes a fatal signal before its grace runs out,
// as a guest that blocks it and waits for it does. The run goes on.
func TestSignalsTakenNotFatal(t *testing.T) {
	signalGrace = 20 * time.Millisecond
	defer func() { signalGrace = 5 * time.Second }()
	run := newTestRun(context.Background())
	s := NewSignals(run)
	defer s.Stop()
	s.Deliver(10) // SIGUSR1, with no handler
	s.Deliver(10) // again: one grace for the signal, not two
	if len(s.grace) != 1 {
		t.Fatalf("%d grace timers", len(s.grace))
	}
	if got := s.take(); got != 1<<10 {
		t.Errorf("took %b", got)
	}
	time.Sleep(10 * signalGrace)
	if run.canceled.Load() || s.Terminated() != 0 {
		t.Errorf("canceled %v, terminated %d", run.canceled.Load(), s.Terminated())
	}
	// Not taken: the grace ends the run.
	s.Deliver(12)
	deadline := time.Now().Add(10 * time.Second)
	for !run.canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Terminated() != 12 {
		t.Errorf("terminated %d, want 12", s.Terminated())
	}
}
