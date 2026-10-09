//go:build windows

//declscope:namespace http

package server

import "strings"

// httpUnsafe reports a URL path Windows would read otherwise than the
// server checked it: a backslash is a separator there, a colon names a
// drive or an alternate data stream (x.php::$DATA is x.php's source), and
// Windows drops a trailing dot or space from a name (x.php. is x.php).
// A tilde before a digit may be an 8.3 short name: GIT~1 is .git, which
// httpHidden would not see.
func httpUnsafe(clean string) bool {
	if strings.ContainsAny(clean, `\:`) {
		return true
	}
	for seg := range strings.SplitSeq(clean, "/") {
		if seg != "" && (strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ")) {
			return true
		}
		for i := 1; i < len(seg); i++ {
			if seg[i-1] == '~' && '0' <= seg[i] && seg[i] <= '9' {
				return true
			}
		}
	}
	return false
}
