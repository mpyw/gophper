//go:build !windows

//declscope:namespace http

package server

import "strings"

// httpUnsafe reports a URL path with a backslash. The host does not read it
// as a separator, but it has no use in a URL path either.
func httpUnsafe(clean string) bool { return strings.Contains(clean, `\`) }
