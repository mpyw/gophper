package gophper_test

import (
	"context"
	"testing"

	"github.com/mpyw/gophper"
)

// newTestEngine returns an Engine closed when the test ends.
//
//declscope:shared // engine_test.go and net_test.go
func newTestEngine(t *testing.T) *gophper.Engine {
	t.Helper()
	e, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}
