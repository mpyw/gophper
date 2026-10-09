package hostvm

import (
	"context"
	"testing"
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
