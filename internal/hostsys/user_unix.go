//go:build !windows

//declscope:namespace user

package hostsys

import (
	"os"
	"os/user"
	"strconv"
)

// userCurrentIDs returns the process's uid, gid and groups.
func userCurrentIDs() (uid, gid uint32, groups []uint32) {
	gs, _ := os.Getgroups()
	for _, g := range gs {
		groups = append(groups, uint32(g))
	}
	return uint32(max(os.Getuid(), 0)), uint32(max(os.Getgid(), 0)), groups
}

// userFindUser returns a user's passwd fields.
func userFindUser(byID bool, id int32, name string) ([]string, bool) {
	u, err := userLookup(byID, id, name)
	if err != nil {
		return nil, false
	}
	return []string{u.Username, "x", u.Uid, u.Gid, u.Name, u.HomeDir, userShell(u)}, true
}

// userFindGroup returns a group's fields. os/user does not list members.
func userFindGroup(byID bool, id int32, name string) ([]string, bool) {
	var g *user.Group
	var err error
	if byID {
		g, err = user.LookupGroupId(strconv.Itoa(int(id)))
	} else {
		g, err = user.LookupGroup(name)
	}
	if err != nil {
		return nil, false
	}
	return []string{g.Name, "x", g.Gid, ""}, true
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
