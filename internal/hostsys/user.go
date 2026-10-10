package hostsys

import (
	"context"
	"os"
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
	uid, gid, groups := userCurrentIDs()
	mem.WriteUint32Le(ids, uid)
	mem.WriteUint32Le(ids+4, gid)
	n := int32(0)
	for _, g := range groups {
		if n == capacity {
			break
		}
		mem.WriteUint32Le(ids+8+uint32(n)*4, g)
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
	var ok bool
	switch kind {
	case userByID, userByName:
		fields, ok = userFindUser(kind == userByID, id, name)
	case userGroupByID, userGroupName:
		fields, ok = userFindGroup(kind == userGroupByID, id, name)
	default:
		return -wasi.EINVAL
	}
	if !ok {
		return -wasi.ENOENT
	}
	text := strings.Join(fields, "\x00") + "\x00"
	if len(text) <= int(capacity) {
		m.Memory().WriteString(out, text)
	}
	return int32(len(text))
}

// userOSHostname is a var so that a test can see os.Hostname fail, as it
// may in a sandbox with neither uname's name nor /proc to read it from.
var userOSHostname = os.Hostname

// userHostName writes the host name, and returns its length or -errno.
//
//declscope:shared // system.go exports it
func userHostName(_ context.Context, m api.Module, out uint32, capacity int32) int32 {
	name, err := userOSHostname()
	if err != nil {
		return -wasi.EIO
	}
	if len(name) > int(capacity) {
		name = name[:capacity]
	}
	m.Memory().WriteString(out, name)
	return int32(len(name))
}
