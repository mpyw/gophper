package gophper

import (
	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper/internal/hostpath"
)

// HostFS returns a file system config that mounts the whole host: "/" on
// Unix, and each drive at /c, /d and so on on Windows. Paths inside PHP
// are then HostToGuest of host paths.
func HostFS() wazero.FSConfig {
	fs := wazero.NewFSConfig()
	for _, r := range hostpath.Roots() {
		fs = fs.WithDirMount(r.Host, r.Guest)
	}
	return fs
}

// HostToGuest returns the path inside PHP for a host path, under HostFS:
// the same path, except on Windows, where D:\app is /d/app.
func HostToGuest(path string) string {
	return hostpath.Guest(path)
}

// HostPaths is an Options.HostPath for HostFS: every path PHP sees is its
// host file, and writable.
func HostPaths(path string) (host string, writable, ok bool) {
	host, ok = hostpath.Host(path)
	return host, ok, ok
}
