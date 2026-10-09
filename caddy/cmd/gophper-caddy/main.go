// Command gophper-caddy is Caddy with gophper built in.
//
//	gophper-caddy php-server --root ./public --domain example.com
//	gophper-caddy run --config Caddyfile
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	_ "github.com/mpyw/gophper/caddy"
)

func main() {
	caddycmd.Main()
}
