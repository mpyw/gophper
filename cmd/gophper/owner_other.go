//go:build !unix

//declscope:namespace main

package main

import "os"

// phpBinaryOwned reports whether this user owns fi. phpBinaryScript writes
// nothing where there is no /bin/sh, so this is never asked.
func phpBinaryOwned(os.FileInfo) bool { return false }
