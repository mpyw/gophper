//declscope:namespace http

package server

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestHTTPBufferBody(t *testing.T) {
	broken := errors.New("broken")
	failAfter := func(n int64) io.Reader {
		return io.MultiReader(io.LimitReader(httpZeros{}, n), httpErrReader{broken})
	}
	// In memory, on disk, and failing in either.
	for _, n := range []int64{10, httpBodyMemory + 10} {
		r, size, cleanup, err := httpBufferBody(io.LimitReader(httpZeros{}, n))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		cleanup()
		if err != nil || size != n || int64(len(got)) != n || !bytes.Equal(got[:10], make([]byte, 10)) {
			t.Errorf("%d bytes: read %d, size %d, %v", n, len(got), size, err)
		}
		if _, _, _, err := httpBufferBody(failAfter(n)); !errors.Is(err, broken) {
			t.Errorf("failing after %d bytes: %v", n, err)
		}
	}
}

type httpZeros struct{}

func (httpZeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type httpErrReader struct{ err error }

func (r httpErrReader) Read([]byte) (int, error) { return 0, r.err }
