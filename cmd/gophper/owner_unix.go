//go:build unix

//declscope:namespace main

package main

import (
	"os"
	"syscall"
)

// phpBinaryOwned reports whether this user owns fi.
func phpBinaryOwned(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
