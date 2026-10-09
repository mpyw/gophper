//go:build unix

//declscope:namespace system

package hostsys

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mpyw/gophper/internal/wasi"
)

// newSystemLocks returns the lock host functions of a fresh instance that
// sees every host path as itself, except "/unmapped".
func newSystemLocks(run guestRun) (*System, systemLockExports) {
	s := NewSystem(run, func(path string) (string, bool, bool) {
		return path, true, path != "/unmapped"
	})
	return s, systemLockExports{from: func(context.Context) *System { return s }}
}

// holdSystemLock takes an exclusive flock on path from outside the instance.
func holdSystemLock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSystemLock(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := newGuest(t)
	ctx := context.Background()
	lock := func(x systemLockExports, fd int32, path string, op int32) int32 {
		ptr, n := putSystemPath(m, path)
		return x.lock(ctx, m, fd, ptr, n, op)
	}

	t.Run("bad requests", func(t *testing.T) {
		_, x := newSystemLocks(guestRun{ctx: ctx})
		for _, tt := range []struct {
			name string
			path string
			op   int32
			want int32
		}{
			{"neither shared nor exclusive", file, systemLockNonblock, wasi.EINVAL},
			{"no host file", "/unmapped", systemLockExclusive, wasi.EINVAL},
			{"missing file", file + ".missing", systemLockExclusive, wasi.EBADF},
			{"unlock without a lock", file, systemLockUnlock, 0},
		} {
			if got := lock(x, 3, tt.path, tt.op); got != tt.want {
				t.Errorf("%s: errno %d, want %d", tt.name, got, tt.want)
			}
		}
	})

	t.Run("busy", func(t *testing.T) {
		holder := holdSystemLock(t, file)
		intr := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		s, x := newSystemLocks(guestRun{ctx: cctx, intr: intr})
		if got := lock(x, 3, file, systemLockShared|systemLockNonblock); got != wasi.EAGAIN {
			t.Errorf("non-blocking: errno %d, want EAGAIN", got)
		}
		close(intr)
		if got := lock(x, 3, file, systemLockExclusive); got != wasi.EINTR {
			t.Errorf("interrupted: errno %d, want EINTR", got)
		}
		s.run = guestRun{ctx: cctx, intr: make(chan struct{})}
		cancel()
		if got := lock(x, 3, file, systemLockExclusive); got != wasi.EIO {
			t.Errorf("after the run: errno %d, want EIO", got)
		}

		// Once the holder lets go, the same fd gets the lock, and Close
		// releases it.
		s.run = guestRun{ctx: ctx, intr: make(chan struct{})}
		syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		if got := lock(x, 3, file, systemLockExclusive); got != 0 {
			t.Fatalf("free: errno %d, want 0", got)
		}
		if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			t.Fatal("the instance's lock did not hold")
		}
		s.Close()
		if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Errorf("Close kept the lock: %v", err)
		}
		if len(s.locks) != 0 {
			t.Errorf("Close left %d files", len(s.locks))
		}
	})

	t.Run("unlock and close", func(t *testing.T) {
		probe, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Close()
		free := func() bool {
			if syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
				return false
			}
			syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
			return true
		}
		_, x := newSystemLocks(guestRun{ctx: ctx})
		if lock(x, 3, file, systemLockExclusive) != 0 || free() {
			t.Fatal("fd 3 did not lock")
		}
		if lock(x, 3, file, systemLockUnlock) != 0 || !free() {
			t.Error("LOCK_UN kept the lock")
		}
		if lock(x, 4, file, systemLockShared) != 0 || free() {
			t.Fatal("fd 4 did not lock")
		}
		if x.unlock(ctx, 4) != 0 || !free() {
			t.Error("closing the fd kept the lock")
		}
		if x.unlock(ctx, 5) != 0 {
			t.Error("closing an fd without a lock failed")
		}
	})
}
