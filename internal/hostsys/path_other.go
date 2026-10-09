//go:build !unix

//declscope:namespace system

package hostsys

import (
	"io/fs"
	"os"
)

// systemOwner reports every file as the current user's, with the uid and
// gid userCurrentIDs gives: the host has no uids.
func systemOwner(fs.FileInfo) (uint32, uint32) {
	uid, gid, _ := userCurrentIDs()
	return uid, gid
}

// systemAccess checks that the file exists, and for writing, that it is not
// read-only. Windows ignores that attribute on a directory, and has no other
// permissions that Go reports.
func systemAccess(path string, mode int32) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode&systemAccessWrite != 0 && !fi.IsDir() && fi.Mode().Perm()&0o200 == 0 {
		return fs.ErrPermission
	}
	return nil
}
