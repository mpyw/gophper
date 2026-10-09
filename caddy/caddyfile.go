package gophpercaddy

import (
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("gophper", caddyfileParse)
	httpcaddyfile.RegisterDirectiveOrder("gophper", httpcaddyfile.Before, "file_server")
}

// UnmarshalCaddyfile reads the gophper directive:
//
//	gophper [<root>] {
//		root     <path>
//		temp_dir <path>
//		workers  <n>
//		ini      <key>=<value> | <key> <value>
//	}
//
// ini can repeat.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the directive name
	if d.NextArg() {
		h.Root = d.Val()
	}
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "root", "temp_dir":
			key := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			if key == "root" {
				h.Root = d.Val()
			} else {
				h.TempDir = d.Val()
			}
		case "workers":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("workers: %v", err)
			}
			h.Workers = n
		case "ini":
			args := d.RemainingArgs()
			switch {
			case len(args) == 1 && strings.Contains(args[0], "="):
				h.INI = append(h.INI, args[0])
			case len(args) == 2:
				h.INI = append(h.INI, args[0]+"="+args[1])
			default:
				return d.ArgErr()
			}
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
	}
	return nil
}

func caddyfileParse(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(Handler)
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

var _ caddyfile.Unmarshaler = (*Handler)(nil)
