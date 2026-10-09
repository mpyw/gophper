//declscope:namespace system

package hostsys

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero/api"
)

// newSystemPaths returns the path host functions for an instance that sees
// every host path as itself, except "/unmapped", and paths under readOnly
// as read-only.
func newSystemPaths(readOnly string) systemPathExports {
	s := NewSystem(guestRun{ctx: context.Background()}, func(path string) (string, bool, bool) {
		if strings.HasPrefix(path, "/unmapped") {
			return "", false, false
		}
		return path, !strings.HasPrefix(path, readOnly), true
	})
	return systemPathExports{from: func(context.Context) *System { return s }}
}

// putSystemPath writes path into memory and returns its pointer and length.
func putSystemPath(m api.Module, path string) (uint32, uint32) {
	m.Memory().WriteString(16, path)
	return 16, uint32(len(path))
}

func TestSystemPathErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix permissions")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	x := newSystemPaths(ro)
	m := newGuest(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		call func(ptr, n uint32) int32
		path string
		want int32
	}{
		{"stat unmapped", func(p, n uint32) int32 { return x.stat(ctx, m, p, n, 1, 512) }, "/unmapped", wasi.ENOENT},
		{"stat missing", func(p, n uint32) int32 { return x.stat(ctx, m, p, n, 1, 512) }, filepath.Join(dir, "missing"), wasi.ENOENT},
		{"access unmapped", func(p, n uint32) int32 { return x.access(ctx, m, p, n, 4) }, "/unmapped", wasi.ENOENT},
		{"access to write read-only", func(p, n uint32) int32 { return x.access(ctx, m, p, n, systemAccessWrite) }, ro, wasi.EROFS},
		{"access to read", func(p, n uint32) int32 { return x.access(ctx, m, p, n, 4) }, file, 0},
		{"access to execute", func(p, n uint32) int32 { return x.access(ctx, m, p, n, systemAccessExecute) }, file, wasi.EACCES},
		{"chmod unmapped", func(p, n uint32) int32 { return x.chmod(ctx, m, p, n, 0o644) }, "/unmapped", wasi.ENOENT},
		{"chmod read-only", func(p, n uint32) int32 { return x.chmod(ctx, m, p, n, 0o644) }, ro, wasi.EROFS},
		{"chmod missing", func(p, n uint32) int32 { return x.chmod(ctx, m, p, n, 0o644) }, filepath.Join(dir, "missing"), wasi.ENOENT},
		{"chown unmapped", func(p, n uint32) int32 { return x.chown(ctx, m, p, n, -1, -1, 1) }, "/unmapped", wasi.ENOENT},
		{"chown read-only", func(p, n uint32) int32 { return x.chown(ctx, m, p, n, -1, -1, 1) }, ro, wasi.EROFS},
		{"chown keeping both", func(p, n uint32) int32 { return x.chown(ctx, m, p, n, -1, -1, 1) }, file, 0},
		{"lchown keeping both", func(p, n uint32) int32 { return x.chown(ctx, m, p, n, -1, -1, 0) }, file, 0},
		{"lchown missing", func(p, n uint32) int32 { return x.chown(ctx, m, p, n, -1, -1, 0) }, filepath.Join(dir, "missing"), wasi.ENOENT},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.call(putSystemPath(m, tt.path)); got != tt.want {
				t.Errorf("errno %d, want %d", got, tt.want)
			}
		})
	}

	t.Run("chown to root", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root may give files away")
		}
		ptr, n := putSystemPath(m, file)
		if got := x.chown(ctx, m, ptr, n, 0, -1, 1); got != wasi.EPERM {
			t.Errorf("errno %d, want EPERM", got)
		}
	})
}

func TestSystemPathStat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix permissions")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	x := newSystemPaths("/nothing-read-only")
	m := newGuest(t)
	ctx := context.Background()
	stat := func(path string, follow int32) [3]uint32 {
		t.Helper()
		ptr, n := putSystemPath(m, path)
		if errno := x.stat(ctx, m, ptr, n, follow, 512); errno != 0 {
			t.Fatalf("stat %s: errno %d", path, errno)
		}
		var out [3]uint32
		for i := range out {
			out[i], _ = m.Memory().ReadUint32Le(512 + uint32(i)*4)
		}
		return out
	}

	ptr, n := putSystemPath(m, file)
	if errno := x.chmod(ctx, m, ptr, n, 0o4751); errno != 0 {
		t.Fatalf("chmod: errno %d", errno)
	}
	if got := stat(file, 1); got[0] != 0o4751 || int(got[1]) != os.Getuid() {
		t.Errorf("file: mode %o, uid %d", got[0], got[1])
	}
	if got := stat(link, 1); got[0] != 0o4751 {
		t.Errorf("followed link: mode %o, want the file's", got[0])
	}
	if got := stat(link, 0); got[0] == 0o4751 {
		t.Errorf("link itself: mode %o, want the link's own", got[0])
	}

	// The setgid bit, which only a member of the file's group may set.
	if st, err := os.Stat(file); err == nil && systemInGroup(t, st) {
		ptr, n := putSystemPath(m, file)
		if errno := x.chmod(ctx, m, ptr, n, 0o2750); errno != 0 {
			t.Fatalf("chmod: errno %d", errno)
		}
		if got := stat(file, 1); got[0] != 0o2750 {
			t.Errorf("setgid file: mode %o, want 2750", got[0])
		}
	}

	// The sticky bit, on a directory.
	sub := filepath.Join(dir, "sticky")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr, n = putSystemPath(m, sub)
	if errno := x.chmod(ctx, m, ptr, n, 0o1777); errno != 0 {
		t.Fatalf("chmod: errno %d", errno)
	}
	if got := stat(sub, 1); got[0] != 0o1777 {
		t.Errorf("sticky directory: mode %o, want 1777", got[0])
	}
}

func TestSystemModeBits(t *testing.T) {
	for _, tt := range []struct {
		in   fs.FileMode
		want uint32
	}{
		{0o644, 0o644},
		{fs.ModeDir | 0o755, 0o755},
		{fs.ModeSetuid | 0o755, 0o4755},
		{fs.ModeSetgid | 0o755, 0o2755},
		{fs.ModeSticky | fs.ModeDir | 0o777, 0o1777},
		{fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o700, 0o7700},
	} {
		if got := systemModeBits(tt.in); got != tt.want {
			t.Errorf("systemModeBits(%v) = %o, want %o", tt.in, got, tt.want)
		}
	}
}

func TestSystemErrno(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int32
	}{
		{nil, 0},
		{&fs.PathError{Op: "stat", Path: "/x", Err: syscall.ENOENT}, wasi.ENOENT},
		{&fs.PathError{Op: "chown", Path: "/x", Err: syscall.EPERM}, wasi.EPERM},
		{&fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}, wasi.EACCES},
		{errors.New("something else"), wasi.EIO},
	} {
		if got := systemErrno(tt.err); got != tt.want {
			t.Errorf("systemErrno(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

// systemFileInfo is a FileInfo with no system data, as a non-OS fs.FS gives.
type systemFileInfo struct{}

func (systemFileInfo) Name() string       { return "f" }
func (systemFileInfo) Size() int64        { return 0 }
func (systemFileInfo) Mode() fs.FileMode  { return 0o644 }
func (systemFileInfo) ModTime() time.Time { return time.Time{} }
func (systemFileInfo) IsDir() bool        { return false }
func (systemFileInfo) Sys() any           { return nil }

func TestSystemOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Every file is the current user's, as userCurrentIDs reports it.
		if uid, gid := systemOwner(systemFileInfo{}); uid != 1000 || gid != 1000 {
			t.Errorf("%d, %d, want 1000, 1000", uid, gid)
		}
		return
	}
	if uid, gid := systemOwner(systemFileInfo{}); uid != 0 || gid != 0 {
		t.Errorf("no system data: %d, %d, want 0, 0", uid, gid)
	}
	st, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if uid, gid := systemOwner(st); int(uid) != os.Getuid() || int(gid) < 0 {
		t.Errorf("own directory: %d, %d", uid, gid)
	}
}

// systemInGroup tells whether the user is in the group of the file st.
func systemInGroup(t *testing.T, st fs.FileInfo) bool {
	t.Helper()
	_, gid := systemOwner(st)
	groups, _ := os.Getgroups()
	return int(gid) == os.Getgid() || slices.Contains(groups, int(gid))
}
