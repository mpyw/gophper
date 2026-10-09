package gophpercaddy

import (
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("gophper", caddyfileParse)
	httpcaddyfile.RegisterDirectiveOrder("gophper", httpcaddyfile.Before, "file_server")
}

// UnmarshalCaddyfile reads the gophper directive:
//
//	gophper [<root>] {
//		root             <path>
//		mount            <path> [ro]
//		router           <path>
//		index            <file...>
//		front_controller <file> | off
//		split_path       <suffix...>
//		file_server      off
//		max_body         <size> | off
//		temp_dir         <path>
//		concurrency      <n>
//		max_wait_time    <duration>
//		php_ini          <key> <value>
//		env              <key> <value>
//	}
//
// mount, php_ini and env can repeat.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the directive name
	if d.NextArg() {
		h.Root = d.Val()
	}
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		key := d.Val()
		args := d.RemainingArgs()
		one := func() (string, error) {
			if len(args) != 1 {
				return "", d.ArgErr()
			}
			return args[0], nil
		}
		var err error
		switch key {
		case "root":
			h.Root, err = one()
		case "router":
			h.Router, err = one()
		case "temp_dir":
			h.TempDir, err = one()
		case "mount":
			switch {
			case len(args) == 1:
				h.Mounts = append(h.Mounts, HandlerMount{Dir: args[0]})
			case len(args) == 2 && args[1] == "ro":
				h.Mounts = append(h.Mounts, HandlerMount{Dir: args[0], ReadOnly: true})
			default:
				return d.ArgErr()
			}
		case "index":
			if len(args) == 0 {
				return d.ArgErr()
			}
			h.Index = args
		case "split_path":
			if len(args) == 0 {
				return d.ArgErr()
			}
			h.SplitPath = args
		case "front_controller":
			var v string
			if v, err = one(); err == nil {
				if v == "off" {
					h.NoFrontController = true
				} else {
					h.FrontController = v
				}
			}
		case "file_server":
			var v string
			if v, err = one(); err == nil {
				if v != "off" {
					return d.Errf("file_server: only off is accepted")
				}
				h.NoStatic = true
			}
		case "max_body":
			var v string
			if v, err = one(); err == nil {
				if v == "off" {
					h.MaxBodySize = -1
				} else {
					size, perr := humanize.ParseBytes(v)
					if perr != nil {
						return d.Errf("max_body: %v", perr)
					}
					h.MaxBodySize = int64(size)
				}
			}
		case "concurrency":
			var v string
			if v, err = one(); err == nil {
				if h.Concurrency, err = strconv.Atoi(v); err != nil {
					return d.Errf("concurrency: %v", err)
				}
			}
		case "max_wait_time":
			var v string
			if v, err = one(); err == nil {
				dur, perr := caddy.ParseDuration(v)
				if perr != nil {
					return d.Errf("max_wait_time: %v", perr)
				}
				h.MaxWaitTime = caddy.Duration(dur)
			}
		case "php_ini", "ini", "env":
			var entry string
			switch {
			case len(args) == 1 && strings.Contains(args[0], "="):
				entry = args[0]
			case len(args) == 2:
				entry = args[0] + "=" + args[1]
			default:
				return d.ArgErr()
			}
			if key == "env" {
				h.Env = append(h.Env, entry)
			} else {
				h.INI = append(h.INI, entry)
			}
		default:
			return d.Errf("unknown subdirective %q", key)
		}
		if err != nil {
			return err
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
