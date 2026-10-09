//go:build !unix

//declscope:namespace main

package main

import (
	"os"
	"path/filepath"
)

// phpBinaryTempBase is in the user's local application data. TMP may name
// a directory others share, such as C:\Windows\Temp for a service.
func phpBinaryTempBase() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gophper"), nil
}
