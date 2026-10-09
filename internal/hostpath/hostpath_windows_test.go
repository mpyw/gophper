//go:build windows

package hostpath

import "testing"

func TestGuestAndHost(t *testing.T) {
	for _, tt := range []struct{ host, guest string }{
		{`D:\a\b`, "/d/a/b"},
		{`C:\`, "/c"},
		{`c:\Users\x y\file.php`, "/c/Users/x y/file.php"},
	} {
		if got := Guest(tt.host); got != tt.guest {
			t.Errorf("Guest(%q) = %q, want %q", tt.host, got, tt.guest)
		}
	}
	for _, tt := range []struct {
		guest, host string
		ok          bool
	}{
		{"/d/a/b", `D:\a\b`, true},
		{"/c", `C:\`, true},
		{"/c/", `C:\`, true},
		{"/", "", false},
		{"/tmp", "", false},
		{"relative", "", false},
	} {
		if got, ok := Host(tt.guest); got != tt.host || ok != tt.ok {
			t.Errorf("Host(%q) = %q, %v, want %q, %v", tt.guest, got, ok, tt.host, tt.ok)
		}
	}
	if len(Roots()) == 0 {
		t.Error("no drive found")
	}
}
