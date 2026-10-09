//go:build !windows

//declscope:namespace hostpath

package hostpath

// Guest returns the path inside PHP for a host path: the same path.
func Guest(host string) string { return host }

// Host returns the host path for a path inside PHP: the same path, clean.
func Host(guest string) (string, bool) { return CleanGuest(guest) }

// Roots returns the host's root, mounted at "/".
func Roots() []Root { return []Root{{Host: "/", Guest: "/"}} }
