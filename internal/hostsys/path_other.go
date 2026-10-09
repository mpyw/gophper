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

// systemAccess checks only that the file exists.
func systemAccess(path string, _ int32) error {
	_, err := os.Stat(path)
	return err
}
