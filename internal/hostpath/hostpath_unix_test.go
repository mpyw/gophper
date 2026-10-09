//go:build !windows

package hostpath

import "testing"

func TestGuestAndHost(t *testing.T) {
	if got := Guest("/a/b"); got != "/a/b" {
		t.Errorf("Guest = %q", got)
	}
	if got, ok := Host("/a/b"); got != "/a/b" || !ok {
		t.Errorf("Host = %q, %v", got, ok)
	}
	if r := Roots(); len(r) != 1 || r[0] != (Root{Host: "/", Guest: "/"}) {
		t.Errorf("Roots = %v", r)
	}
}
