//go:build unix

//declscope:namespace system

package hostsys

import (
	"errors"
	"os"
	"syscall"
)

// errSystemLockBusy is the error of a non-blocking attempt on a held lock.
var errSystemLockBusy = errors.New("hostsys: the file is locked")

// systemLockFile tries a flock(2) operation without waiting.
func systemLockFile(f *os.File, op int32) error {
	how := syscall.LOCK_UN
	switch op {
	case systemLockShared:
		how = syscall.LOCK_SH | syscall.LOCK_NB
	case systemLockExclusive:
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := sc.Control(func(fd uintptr) { ferr = syscall.Flock(int(fd), how) }); err != nil {
		return err
	}
	if errors.Is(ferr, syscall.EWOULDBLOCK) {
		return errSystemLockBusy
	}
	return ferr
}
