package hostsys

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/mpyw/gophper/internal/wasi"
	"github.com/tetratelabs/wazero/api"
)

// What user_get looks up. Keep in sync with gophper-wasm (ABI.md).
const (
	userByID      int32 = 1
	userByName    int32 = 2
	userGroupByID int32 = 3
	userGroupName int32 = 4
)

// userIDs writes the uid, the gid and up to capacity group ids, and
// returns the number of groups.
//
//declscope:shared // system.go exports it
func userIDs(_ context.Context, m api.Module, ids uint32, capacity int32) int32 {
	mem := m.Memory()
	mem.WriteUint32Le(ids, uint32(max(os.Getuid(), 0)))
	mem.WriteUint32Le(ids+4, uint32(max(os.Getgid(), 0)))
	groups, _ := os.Getgroups()
	n := int32(0)
	for _, g := range groups {
		if n == capacity {
			break
		}
		mem.WriteUint32Le(ids+8+uint32(n)*4, uint32(g))
		n++
	}
	return n
}

// userGet writes an entry as NUL-separated fields. For a user: name,
// password, uid, gid, gecos, home and shell. For a group: name, password,
// gid and the members, joined by commas. It returns the length, which may
// exceed capacity to ask for more room, or -ENOENT.
//
//declscope:shared // system.go exports it
func userGet(_ context.Context, m api.Module, kind, id int32, namePtr, nameLen, out uint32, capacity int32) int32 {
	b, _ := m.Memory().Read(namePtr, nameLen)
	name := string(b)
	var fields []string
	switch kind {
	case userByID, userByName:
		u, err := userLookup(kind == userByID, id, name)
		if err != nil {
			return -wasi.ENOENT
		}
		fields = []string{u.Username, "x", u.Uid, u.Gid, u.Name, u.HomeDir, userShell(u)}
	case userGroupByID, userGroupName:
		var g *user.Group
		var err error
		if kind == userGroupByID {
			g, err = user.LookupGroupId(strconv.Itoa(int(id)))
		} else {
			g, err = user.LookupGroup(name)
		}
		if err != nil {
			return -wasi.ENOENT
		}
		// os/user does not list members.
		fields = []string{g.Name, "x", g.Gid, ""}
	default:
		return -wasi.EINVAL
	}
	text := strings.Join(fields, "\x00") + "\x00"
	if len(text) <= int(capacity) {
		m.Memory().WriteString(out, text)
	}
	return int32(len(text))
}

// userLookup finds a user. Without cgo, os/user reads only /etc/passwd,
// which on macOS lacks real users. The current user is then taken from
// user.Current, which falls back to $USER and $HOME.
func userLookup(byID bool, id int32, name string) (*user.User, error) {
	var u *user.User
	var err error
	if byID {
		u, err = user.LookupId(strconv.Itoa(int(id)))
	} else {
		u, err = user.Lookup(name)
	}
	if err == nil {
		return u, nil
	}
	cur, cerr := user.Current()
	if cerr != nil {
		return nil, err
	}
	if (byID && cur.Uid == strconv.Itoa(int(id))) || (!byID && cur.Username == name) {
		return cur, nil
	}
	return nil, err
}

// userShell guesses the login shell, which os/user does not report.
func userShell(u *user.User) string {
	if cur, err := user.Current(); err == nil && cur.Uid == u.Uid {
		if sh := os.Getenv("SHELL"); sh != "" {
			return sh
		}
	}
	return "/bin/sh"
}

// userHostName writes the host name, and returns its length or -errno.
//
//declscope:shared // system.go exports it
func userHostName(_ context.Context, m api.Module, out uint32, capacity int32) int32 {
	name, err := os.Hostname()
	if err != nil {
		return -wasi.EIO
	}
	if len(name) > int(capacity) {
		name = name[:capacity]
	}
	m.Memory().WriteString(out, name)
	return int32(len(name))
}
