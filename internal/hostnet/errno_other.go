//go:build !windows

//declscope:namespace errno

package hostnet

// errnoByPlatform is empty: syscall's numbers are the host's.
var errnoByPlatform []errnoMapping
