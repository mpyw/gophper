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

func TestCleanGuest(t *testing.T) {
	for _, tt := range []struct {
		in, out string
		ok      bool
	}{
		{"/a/../../etc/x", "/etc/x", true},
		{"/tmp/./x/", "/tmp/x", true},
		{"relative", "", false},
		{"", "", false},
	} {
		if got, ok := CleanGuest(tt.in); got != tt.out || ok != tt.ok {
			t.Errorf("CleanGuest(%q) = %q, %v, want %q, %v", tt.in, got, ok, tt.out, tt.ok)
		}
	}
}
