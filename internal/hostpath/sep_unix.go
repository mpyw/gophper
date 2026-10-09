//go:build !windows

//declscope:namespace hostpath

package hostpath

// hostpathForeignSeparator reports nothing: on Unix, "/" is the only one.
func hostpathForeignSeparator(string) bool { return false }
