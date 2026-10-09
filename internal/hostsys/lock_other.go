//go:build !unix && !windows

//declscope:namespace system

package hostsys

import (
	"errors"
	"os"
)

// errSystemLockBusy is the error of a non-blocking attempt on a held lock.
var errSystemLockBusy = errors.New("hostsys: the file is locked")

// systemLockFile fails: this host has no file locks gophper knows of. A
// lock that succeeded without excluding anyone would let two writers in.
func systemLockFile(*os.File, int32) error { return errors.ErrUnsupported }
