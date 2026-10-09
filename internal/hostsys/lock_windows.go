//go:build windows

//declscope:namespace system

package hostsys

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// errSystemLockBusy is the error of a non-blocking attempt on a held lock.
var errSystemLockBusy = errors.New("hostsys: the file is locked")

// systemLockOffset is where the lock lies: past any end of file. Windows
// locks are mandatory, so a lock on the file's bytes would also fail PHP's
// own reads and writes, which go through another handle. A lock past the
// end excludes other lockers only, as flock(2) does.
const systemLockOffset = 1<<63 - 1

// systemLockFile tries a lock with LockFileEx, without waiting.
func systemLockFile(f *os.File, op int32) error {
	h := windows.Handle(f.Fd())
	ov := &windows.Overlapped{Offset: uint32(systemLockOffset & 0xffffffff), OffsetHigh: uint32(systemLockOffset >> 32)}
	if op != systemLockShared && op != systemLockExclusive {
		err := windows.UnlockFileEx(h, 0, 1, 0, ov)
		if errors.Is(err, windows.ERROR_NOT_LOCKED) {
			return nil
		}
		return err
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if op == systemLockExclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(h, flags, 0, 1, 0, ov)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errSystemLockBusy
	}
	return err
}
