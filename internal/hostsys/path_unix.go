//go:build unix

//declscope:namespace system

package hostsys

import (
	"io/fs"
	"syscall"
)

// systemOwner returns the uid and gid of a file.
func systemOwner(st fs.FileInfo) (uint32, uint32) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Uid, s.Gid
	}
	return 0, 0
}

// systemAccess is access(2) on the host.
func systemAccess(path string, mode int32) error {
	return syscall.Access(path, uint32(mode))
}
