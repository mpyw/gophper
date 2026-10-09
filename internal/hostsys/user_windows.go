//go:build windows

//declscope:namespace user

package hostsys

import (
	"os/user"
	"strconv"
	"strings"

	"github.com/mpyw/gophper/internal/hostpath"
)

// userWindowsID is the uid and gid PHP sees for the current user. Windows
// has security IDs, not numbers. PHP, as on Unix, then sees one user that
// is not root, and owns the files it creates.
const userWindowsID = 1000

// userCurrentIDs returns userWindowsID, and no other groups.
//
//declscope:shared // path_other.go reports every file as this user's
func userCurrentIDs() (uid, gid uint32, groups []uint32) {
	return userWindowsID, userWindowsID, nil
}

// userCurrent returns the current user, with the name PHP sees: without
// the domain, as $USER would be.
func userCurrent() (*user.User, string, bool) {
	u, err := user.Current()
	if err != nil {
		return nil, "", false
	}
	short := u.Username
	if i := strings.LastIndexByte(short, '\\'); i >= 0 {
		short = short[i+1:]
	}
	return u, short, true
}

// userFindUser knows the current user only, by userWindowsID or by name.
func userFindUser(byID bool, id int32, name string) ([]string, bool) {
	u, short, ok := userCurrent()
	if !ok || (byID && id != userWindowsID) || (!byID && name != short && name != u.Username) {
		return nil, false
	}
	uid := strconv.Itoa(userWindowsID)
	return []string{short, "x", uid, uid, u.Name, hostpath.Guest(u.HomeDir), "/bin/sh"}, true
}

// userFindGroup knows the current user's primary group only.
func userFindGroup(byID bool, id int32, name string) ([]string, bool) {
	u, short, ok := userCurrent()
	if !ok {
		return nil, false
	}
	gname := "users"
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		gname = g.Name
	}
	if (byID && id != userWindowsID) || (!byID && name != gname) {
		return nil, false
	}
	return []string{gname, "x", strconv.Itoa(userWindowsID), short}, true
}
