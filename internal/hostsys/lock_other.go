//go:build !unix

//declscope:namespace system

package hostsys

import (
	"errors"
	"os"
)

// errSystemLockBusy is the error of a non-blocking attempt on a held lock.
var errSystemLockBusy = errors.New("hostsys: the file is locked")

// systemLockFile always succeeds: only Unix hosts lock files for now.
func systemLockFile(*os.File, int32) error { return nil }
