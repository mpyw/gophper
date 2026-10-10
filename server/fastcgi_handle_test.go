//declscope:namespace fastcgi

package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/internal/fcgi"
)

// fastcgiGone fails every write, as FCGI_STDOUT does once the web server
// has gone.
type fastcgiGone struct{}

func (fastcgiGone) Write([]byte) (int, error) { return 0, errors.New("the web server is gone") }

// TestFastCGIPingStatusGone reports the ping and status requests as failed
// when their answer cannot be written.
func TestFastCGIPingStatusGone(t *testing.T) {
	engine, err := gophper.NewEngine(context.Background(), gophper.DefaultEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	s, err := NewFastCGIServer(engine, FastCGIConfig{
		PHPConfig: PHPConfig{Mounts: []Mount{{Dir: t.TempDir()}}, TempDir: t.TempDir(), NoWorkers: true, NoOpcache: true, ErrorLog: io.Discard},
		PingPath:  "/ping", StatusPath: "/status",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, uri := range []string{"/ping", "/status"} {
		r := &fcgi.Request{Params: map[string]string{"REQUEST_URI": uri}, Stdin: strings.NewReader(""), Stdout: fastcgiGone{}, Stderr: io.Discard}
		if got := s.handle(context.Background(), r); got != 1 {
			t.Errorf("%s: status %d, want 1", uri, got)
		}
	}
}
