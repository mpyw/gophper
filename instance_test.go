//declscope:namespace engine

package gophper

import (
	"context"
	"testing"
)

// TestEngineFromOutside finds no instance for a host call made outside a
// run, and returns the zero value rather than a part of another run.
func TestEngineFromOutside(t *testing.T) {
	if got := engineFrom(func(*engineInstance) int { return 1 })(context.Background()); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}
