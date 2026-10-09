package hostsys

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"

	"github.com/mpyw/gophper/internal/wasi"
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

func TestUserGet(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("no current user:", err)
	}
	uid, _ := strconv.Atoi(cur.Uid)
	gid, _ := strconv.Atoi(cur.Gid)
	t.Setenv("SHELL", "/bin/test-shell")
	for _, tt := range []struct {
		name string
		kind int32
		id   int32
		key  string
		want []string // nil: not found
	}{
		{"user by id", userByID, int32(uid), "", []string{cur.Username, "x", cur.Uid, cur.Gid, cur.Name, cur.HomeDir, "/bin/test-shell"}},
		{"user by name", userByName, 0, cur.Username, []string{cur.Username, "x", cur.Uid, cur.Gid, cur.Name, cur.HomeDir, "/bin/test-shell"}},
		{"missing user id", userByID, 987654, "", nil},
		{"missing user name", userByName, 0, "gophper-no-such-user", nil},
		{"missing group id", userGroupByID, 987654, "", nil},
		{"missing group name", userGroupName, 0, "gophper-no-such-group", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n, got := getUser(t, tt.kind, tt.id, tt.key, 4096)
			if tt.want == nil {
				if n != -wasi.ENOENT {
					t.Errorf("got %d %q, want -ENOENT", n, got)
				}
				return
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("group by id and name", func(t *testing.T) {
		g, err := user.LookupGroupId(cur.Gid)
		if err != nil {
			t.Skip("the primary group has no name:", err)
		}
		want := g.Name + "|x|" + g.Gid + "|"
		if _, got := getUser(t, userGroupByID, int32(gid), "", 4096); strings.Join(got, "|") != want {
			t.Errorf("by id: got %q, want %q", got, want)
		}
		if _, got := getUser(t, userGroupName, 0, g.Name, 4096); strings.Join(got, "|") != want {
			t.Errorf("by name: got %q, want %q", got, want)
		}
	})

	t.Run("too small", func(t *testing.T) {
		full, _ := getUser(t, userByID, int32(uid), "", 4096)
		if n, got := getUser(t, userByID, int32(uid), "", 4); n != full || got != nil {
			t.Errorf("got %d %q, want the length %d to ask for more room", n, got, full)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		if n, _ := getUser(t, 99, 0, "", 4096); n != -wasi.EINVAL {
			t.Errorf("got %d, want -EINVAL", n)
		}
	})
}

func TestUserShell(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("no current user:", err)
	}
	t.Setenv("SHELL", "")
	if got := userShell(cur); got != "/bin/sh" {
		t.Errorf("without $SHELL: %q, want /bin/sh", got)
	}
	t.Setenv("SHELL", "/bin/test-shell")
	if got := userShell(&user.User{Uid: "987654"}); got != "/bin/sh" {
		t.Errorf("another user: %q, want /bin/sh", got)
	}
}

func TestUserLookupCurrent(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("no current user:", err)
	}
	// The current user is found by both keys, even where os/user cannot
	// read the user database.
	uid, _ := strconv.Atoi(cur.Uid)
	if u, err := userLookup(true, int32(uid), ""); err != nil || u.Username != cur.Username {
		t.Errorf("by id: %v, %v", u, err)
	}
	if u, err := userLookup(false, 0, cur.Username); err != nil || u.Uid != cur.Uid {
		t.Errorf("by name: %v, %v", u, err)
	}
}

func TestUserIDsAndHostName(t *testing.T) {
	m := newGuest(t)
	mem := m.Memory()
	const out = 64
	groups, _ := os.Getgroups()
	if n := userIDs(context.Background(), m, out, 0); n != 0 {
		t.Errorf("no room for groups: %d", n)
	}
	if uid, _ := mem.ReadUint32Le(out); int(uid) != max(os.Getuid(), 0) {
		t.Errorf("uid %d", uid)
	}
	if n := userIDs(context.Background(), m, out, 1000); int(n) != len(groups) {
		t.Errorf("groups: %d, want %d", n, len(groups))
	}

	name, err := os.Hostname()
	if err != nil {
		t.Skip(err)
	}
	if n := userHostName(context.Background(), m, out, 255); n != int32(len(name)) {
		t.Errorf("host name length %d, want %d", n, len(name))
	} else if b, _ := mem.Read(out, uint32(n)); string(b) != name {
		t.Errorf("host name %q, want %q", b, name)
	}
	if n := userHostName(context.Background(), m, out, 1); n != 1 {
		t.Errorf("truncated host name: %d, want 1", n)
	}
}
