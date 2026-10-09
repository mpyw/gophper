package hostsig

import (
	"context"
	"testing"
)

type testRun struct{ ctx context.Context }

func (r *testRun) Context() context.Context      { return r.ctx }
func (r *testRun) Interruption() <-chan struct{} { return nil }
func (r *testRun) Wake()                         {}
func (r *testRun) Cancel()                       {}

// TestSignalsStop cancels the timers a run left, so none fires on a later run.
func TestSignalsStop(t *testing.T) {
	s := NewSignals(&testRun{ctx: context.Background()})
	s.Deliver(15) // SIGTERM, fatal without a handler
	if s.SetAlarm(60) != 0 {
		t.Error("an alarm was already set")
	}
	if len(s.grace) != 1 || s.alarm == nil {
		t.Fatalf("grace %d, alarm %v", len(s.grace), s.alarm)
	}
	grace, alarm := s.grace[0], s.alarm
	s.Stop()
	if grace.Stop() || alarm.Stop() {
		t.Error("a timer was still running")
	}
	s.Deliver(15)
	if s.SetAlarm(60) != 0 || len(s.grace) != 0 || s.alarm != nil {
		t.Errorf("a timer started after Stop: grace %d, alarm %v", len(s.grace), s.alarm)
	}
}
