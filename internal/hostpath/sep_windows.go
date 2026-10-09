//go:build windows

//declscope:namespace hostpath

package hostpath

import "strings"

// hostpathForeignSeparator reports a "\\", which Windows reads as a
// separator, or a ":", which it reads as a drive or a stream.
func hostpathForeignSeparator(p string) bool { return strings.ContainsAny(p, `\:`) }
