//go:build windows

//declscope:namespace http

package server

import "strings"

// httpUnsafe reports a URL path Windows would read otherwise than the
// server checked it: a backslash is a separator there, a colon names a
// drive or an alternate data stream (x.php::$DATA is x.php's source), and
// Windows drops a trailing dot or space from a name (x.php. is x.php).
func httpUnsafe(clean string) bool {
	if strings.ContainsAny(clean, `\:`) {
		return true
	}
	for seg := range strings.SplitSeq(clean, "/") {
		if seg != "" && (strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ")) {
			return true
		}
	}
	return false
}
