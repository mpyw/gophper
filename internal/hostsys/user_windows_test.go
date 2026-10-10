//go:build windows

//declscope:namespace user

package hostsys

import (
	"context"
	"errors"
	"os/user"
	"strings"
	"testing"
)

// getUser calls user_get and returns its result and the fields it wrote.
func getUser(t *testing.T, kind, id int32, name string, capacity int32) (int32, []string) {
	t.Helper()
	m := newGuest(t)
	const namePtr, out = 16, 1024
	m.Memory().WriteString(namePtr, name)
	n := userGet(context.Background(), m, kind, id, namePtr, uint32(len(name)), out, capacity)
	if n <= 0 || n > capacity {
		return n, nil
	}
	b, _ := m.Memory().Read(out, uint32(n))
	return n, strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// TestUserWindows: the current user is uid and gid 1000, by id and by
// name, and no other user exists.
func TestUserWindows(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	short := cur.Username[strings.LastIndexByte(cur.Username, '\\')+1:]
	_, byID := getUser(t, userByID, userWindowsID, "", 4096)
	if len(byID) != 7 || byID[0] != short || byID[2] != "1000" || byID[3] != "1000" || !strings.HasPrefix(byID[5], "/") {
		t.Errorf("by id: %q", byID)
	}
	if _, byName := getUser(t, userByName, 0, short, 4096); len(byName) != 7 || byName[2] != "1000" {
		t.Errorf("by name: %q", byName)
	}
	if n, _ := getUser(t, userByID, 0, "", 4096); n >= 0 {
		t.Errorf("uid 0 exists: %d", n)
	}
	if _, g := getUser(t, userGroupByID, userWindowsID, "", 4096); len(g) != 4 || g[2] != "1000" || g[3] != short {
		t.Errorf("group: %q", g)
	}
	// Only the current user and its group exist.
	if _, ok := userFindUser(false, 0, "gophper-no-such-user"); ok {
		t.Error("another user by name")
	}
	if _, ok := userFindGroup(false, 0, "gophper-no-such-group"); ok {
		t.Error("another group by name")
	}
	if _, ok := userFindGroup(true, 0, ""); ok {
		t.Error("gid 0 exists")
	}
	if uid, gid, groups := userCurrentIDs(); uid != 1000 || gid != 1000 || len(groups) != 0 {
		t.Errorf("ids %d %d %v", uid, gid, groups)
	}
}

// TestUserWindowsNoCurrent: without the current user, no user or group
// exists.
func TestUserWindowsNoCurrent(t *testing.T) {
	current := userOSCurrent
	t.Cleanup(func() { userOSCurrent = current })
	userOSCurrent = func() (*user.User, error) { return nil, errors.New("no token") }
	if _, ok := userFindUser(true, userWindowsID, ""); ok {
		t.Error("a user without the current one")
	}
	if _, ok := userFindGroup(true, userWindowsID, ""); ok {
		t.Error("a group without the current one")
	}
}
