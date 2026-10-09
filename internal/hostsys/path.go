//declscope:namespace system

package hostsys

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/tetratelabs/wazero/api"
)

// access(2) modes, as wasi-libc's <unistd.h> numbers them.
const (
	systemAccessExecute int32 = 1
	systemAccessWrite   int32 = 2
)

// systemPathExports are the host functions that WASI lacks for paths:
// permissions, owners and access checks, done on the host file.
type systemPathExports struct {
	from func(context.Context) *System
}

func (x systemPathExports) resolve(ctx context.Context, m api.Module, ptr, n uint32) (string, bool, bool) {
	b, _ := m.Memory().Read(ptr, n)
	return x.from(ctx).hostPath(string(b))
}

// stat writes the host file's mode bits, uid and gid, each a u32. Only
// these: WASI reports the rest itself.
func (x systemPathExports) stat(ctx context.Context, m api.Module, pathPtr, pathLen uint32, follow int32, out uint32) int32 {
	host, _, ok := x.resolve(ctx, m, pathPtr, pathLen)
	if !ok {
		return errnoENOENT
	}
	var st fs.FileInfo
	var err error
	if follow != 0 {
		st, err = os.Stat(host)
	} else {
		st, err = os.Lstat(host)
	}
	if err != nil {
		return systemErrno(err)
	}
	uid, gid := systemOwner(st)
	mem := m.Memory()
	mem.WriteUint32Le(out, systemModeBits(st.Mode()))
	mem.WriteUint32Le(out+4, uid)
	mem.WriteUint32Le(out+8, gid)
	return 0
}

// access checks access(2) modes against the host file.
func (x systemPathExports) access(ctx context.Context, m api.Module, pathPtr, pathLen uint32, mode int32) int32 {
	host, writable, ok := x.resolve(ctx, m, pathPtr, pathLen)
	switch {
	case !ok:
		return errnoENOENT
	case mode&systemAccessWrite != 0 && !writable:
		return errnoEROFS
	}
	return systemErrno(systemAccess(host, mode))
}

func (x systemPathExports) chmod(ctx context.Context, m api.Module, pathPtr, pathLen uint32, mode int32) int32 {
	host, writable, ok := x.resolve(ctx, m, pathPtr, pathLen)
	switch {
	case !ok:
		return errnoENOENT
	case !writable:
		return errnoEROFS
	}
	fm := fs.FileMode(mode) & fs.ModePerm
	if mode&0o4000 != 0 {
		fm |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		fm |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		fm |= fs.ModeSticky
	}
	return systemErrno(os.Chmod(host, fm))
}

// chown changes the owner. -1 keeps the uid or the gid, as chown(2) does.
func (x systemPathExports) chown(ctx context.Context, m api.Module, pathPtr, pathLen uint32, uid, gid, follow int32) int32 {
	host, writable, ok := x.resolve(ctx, m, pathPtr, pathLen)
	switch {
	case !ok:
		return errnoENOENT
	case !writable:
		return errnoEROFS
	}
	if follow != 0 {
		return systemErrno(os.Chown(host, int(uid), int(gid)))
	}
	return systemErrno(os.Lchown(host, int(uid), int(gid)))
}

// systemModeBits converts a FileMode to the permission bits of st_mode.
func systemModeBits(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

func systemErrno(err error) int32 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, fs.ErrNotExist):
		return errnoENOENT
	case errors.Is(err, syscall.EPERM):
		// chown and chmod of a file the user does not own. fs.ErrPermission
		// matches it too, but "Permission denied" would be the wrong message.
		return errnoEPERM
	case errors.Is(err, fs.ErrPermission):
		return errnoEACCES
	}
	return errnoEIO
}
