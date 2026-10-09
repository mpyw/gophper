//declscope:namespace system

package hostsys

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero/api"
)

// flock(2) operations, as wasi-libc's <sys/file.h> numbers them.
const (
	systemLockShared    int32 = 1
	systemLockExclusive int32 = 2
	systemLockNonblock  int32 = 4
	systemLockUnlock    int32 = 8
)

// systemLockPoll is how often a blocking flock tries again. Trying again, rather
// than one blocking call, lets an interrupt or the end of the run stop it.
const systemLockPoll = 10 * time.Millisecond

// systemLockExports are the file_lock and file_unlock host functions.
type systemLockExports struct {
	from func(context.Context) *System
}

// lock does flock(2) on the host file at path, for the guest fd. Each guest
// fd has its own host file, so two fds conflict as two open files do.
func (x systemLockExports) lock(ctx context.Context, m api.Module, fd int32, pathPtr, pathLen uint32, op int32) int32 {
	s := x.from(ctx)
	b, _ := m.Memory().Read(pathPtr, pathLen)
	path := string(b)

	s.mu.Lock()
	f := s.locks[fd]
	s.mu.Unlock()
	if op&systemLockUnlock != 0 {
		if f != nil {
			// Closing the file releases the lock too, so neither error
			// changes anything for the guest.
			_ = systemLockFile(f, systemLockUnlock)
			_ = f.Close()
			s.mu.Lock()
			delete(s.locks, fd)
			s.mu.Unlock()
		}
		return 0
	}
	if op&(systemLockShared|systemLockExclusive) == 0 {
		return wasi.EINVAL
	}
	opened := f == nil
	if opened {
		host, _, ok := s.hostPath(path)
		if !ok {
			// No host file: nothing else can hold the lock.
			return wasi.EINVAL
		}
		var err error
		if f, err = os.Open(host); err != nil {
			return wasi.EBADF
		}
	}
	keep := func() {
		s.mu.Lock()
		s.locks[fd] = f
		s.mu.Unlock()
	}
	intr := s.run.Interruption()
	for {
		err := systemLockFile(f, op&(systemLockShared|systemLockExclusive))
		switch {
		case err == nil:
			keep()
			return 0
		case !errors.Is(err, errSystemLockBusy):
			if opened {
				_ = f.Close() // The lock error wins.
			}
			// A file kept from before stays, for unlock and close to find.
			return wasi.EIO
		case op&systemLockNonblock != 0:
			keep()
			return wasi.EAGAIN
		}
		select {
		case <-time.After(systemLockPoll):
		case <-intr:
			keep()
			return wasi.EINTR
		case <-s.run.Context().Done():
			keep()
			return wasi.EIO
		}
	}
}

// unlock releases the lock of a guest fd that is being closed.
func (x systemLockExports) unlock(ctx context.Context, fd int32) int32 {
	s := x.from(ctx)
	s.mu.Lock()
	f := s.locks[fd]
	delete(s.locks, fd)
	s.mu.Unlock()
	if f != nil {
		_ = f.Close() // Closing releases the lock; its error changes nothing.
	}
	return 0
}
