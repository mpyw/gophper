//go:build !unix

//declscope:namespace system

package hostsys

import (
	"io/fs"
	"os"
)

// systemOwner returns 0: only Unix hosts have uids.
func systemOwner(fs.FileInfo) (uint32, uint32) { return 0, 0 }

// systemAccess checks only that the file exists.
func systemAccess(path string, _ int32) error {
	_, err := os.Stat(path)
	return err
}
