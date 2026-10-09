package gophpercaddy

import (
	"reflect"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
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
			root /srv/public
			mount /srv ro
			mount /var/data
			router router.php
			index index.php index.html
			front_controller off
			split_path .php .phtml
			file_server off
			max_body 10MB
			temp_dir /var/tmp
			concurrency 8
			max_wait_time 5s
			php_ini display_errors 0
			php_ini max_execution_time=30
			env APP_ENV production
		}`, Handler{
			Root:              "/srv/public",
			Mounts:            []HandlerMount{{Dir: "/srv", ReadOnly: true}, {Dir: "/var/data"}},
			Router:            "router.php",
			Index:             []string{"index.php", "index.html"},
			NoFrontController: true,
			SplitPath:         []string{".php", ".phtml"},
			NoStatic:          true,
			MaxBodySize:       10_000_000,
			TempDir:           "/var/tmp",
			Concurrency:       8,
			MaxWaitTime:       caddy.Duration(5 * time.Second),
			INI:               []string{"display_errors=0", "max_execution_time=30"},
			Env:               []string{"APP_ENV=production"},
		}, false},
		{"php settings", `gophper {
			php_ini_file /etc/php.ini
			opcache /var/cache/opcache
			workers off
			max_requests 100
			processes off
		}`, Handler{INIFile: "/etc/php.ini", OpcacheDir: "/var/cache/opcache", NoWorkers: true, MaxRequests: 100, NoProcesses: true}, false},
		{"opcache off", `gophper {
			opcache off
		}`, Handler{NoOpcache: true}, false},
		{"front controller and max_body off", `gophper {
			front_controller app.php
			max_body off
		}`, Handler{FrontController: "app.php", MaxBodySize: -1}, false},
		{"two arguments", `gophper a b`, Handler{}, true},
		{"bad concurrency", `gophper {
			concurrency many
		}`, Handler{}, true},
		{"file_server on", `gophper {
			file_server on
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
