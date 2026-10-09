//declscope:namespace http

package server

import (
	"runtime"
	"testing"
)

func TestHTTPUnsafe(t *testing.T) {
	windows := runtime.GOOS == "windows"
	for path, unsafe := range map[string]bool{
		"/index.php":          false,
		"/a/b~c/file~.txt":    false,
		`/a\..\secret.php`:    true,
		"/x.php::$DATA":       windows,
		"/x.php.":             windows,
		"/x.php ":             windows,
		"/GIT~1/config":       windows,
		"/dir/ENV~12":         windows,
		"/C:/Windows/win.ini": windows,
	} {
		if got := httpUnsafe(path); got != unsafe {
			t.Errorf("httpUnsafe(%q) = %v, want %v", path, got, unsafe)
		}
	}
}
