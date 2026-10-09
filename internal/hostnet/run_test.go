package hostnet

import (
	"testing"
	"time"
)

// An interrupt cancels the context, so a dial or a lookup returns.
func TestInterruptibleRunContextOnInterrupt(t *testing.T) {
	run := newGuestRun(t)
	ctx, cancel := interruptibleRunContext(run)
	defer cancel()
	close(run.intr)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the interrupt did not cancel the context")
	}
}
