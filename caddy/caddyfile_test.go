package gophpercaddy

import (
	"reflect"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        Handler
		wantErr     bool
	}{
		{"bare", `gophper`, Handler{}, false},
		{"root argument", `gophper ./public`, Handler{Root: "./public"}, false},
		{"block", `gophper {
			root /srv
			temp_dir /var/tmp
			workers 8
			ini display_errors 0
			ini max_execution_time=30
		}`, Handler{Root: "/srv", TempDir: "/var/tmp", Workers: 8, INI: []string{"display_errors=0", "max_execution_time=30"}}, false},
		{"two arguments", `gophper a b`, Handler{}, true},
		{"bad workers", `gophper {
			workers many
		}`, Handler{}, true},
		{"unknown", `gophper {
			nope
		}`, Handler{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got Handler
			err := got.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
