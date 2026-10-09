//go:build !unix

//declscope:namespace main

package main

import "os"

// phpBinaryOwned reports whether this user owns fi. On Windows,
// phpBinaryBase does not ask: the user's local application data is private.
func phpBinaryOwned(os.FileInfo) bool { return false }
