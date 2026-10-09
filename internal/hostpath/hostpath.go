//declscope:namespace hostpath

// Package hostpath maps host paths to the paths PHP sees, and back.
//
// PHP runs in wasm, where every path is a POSIX one. On Unix, a host path
// is the same inside PHP. On Windows, a drive appears as a directory at
// the root, as in Git Bash and MSYS: D:\app\index.php is /d/app/index.php.
package hostpath

// Root is a host directory mounted at a path inside PHP, to reach every
// host path: "/" on Unix, and each drive on Windows.
type Root struct {
	Host, Guest string
}
