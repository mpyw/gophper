package gophpercaddy

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/certmagic"
	"github.com/spf13/cobra"
)

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "php-server",
		Usage: "[--domain <example.com>] [--root <path>] [--listen <addr>] [--mount <dir>]... [--router <file>] [--concurrency <n>] [--ini <key=value>]... [--env <key=value>]...",
		Short: "Serves a PHP app, with no config file",
		Long: `
Serves the PHP app in --root (default: the current directory). Existing
.php files run, other files are sent as they are, and every other path runs
the root's index.php.

With --domain, the server listens on the HTTPS port and gets a certificate
automatically. Point the domain's DNS records at this machine first.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.Flags().StringP("domain", "d", "", "Domain name to serve, with automatic HTTPS")
			cmd.Flags().StringP("root", "r", ".", "The document root")
			cmd.Flags().StringP("listen", "l", "", "The address to listen on (default :80, or :443 with --domain)")
			cmd.Flags().StringSlice("mount", nil, "A directory PHP may access, DIR or DIR:ro (repeatable, default: the root)")
			cmd.Flags().String("router", "", "A script that runs for every request, as with php -S")
			cmd.Flags().Int("concurrency", 0, "Max PHP instances at once (default: the number of CPUs)")
			cmd.Flags().StringSlice("ini", nil, "A php.ini entry as key=value (repeatable)")
			cmd.Flags().StringSlice("env", nil, "An environment variable for PHP as KEY=VALUE (repeatable)")
			cmd.Flags().BoolP("access-log", "a", false, "Enable the access log")
			cmd.RunE = caddycmd.WrapCommandFuncForCobra(commandPHPServer)
		},
	})
}

func commandPHPServer(fl caddycmd.Flags) (int, error) {
	domain := fl.String("domain")
	listen := fl.String("listen")
	ini, err := fl.GetStringSlice("ini")
	if err != nil {
		return caddy.ExitCodeFailedStartup, err
	}
	concurrency, err := fl.GetInt("concurrency")
	if err != nil {
		return caddy.ExitCodeFailedStartup, err
	}
	envs, err := fl.GetStringSlice("env")
	if err != nil {
		return caddy.ExitCodeFailedStartup, err
	}
	mountFlags, err := fl.GetStringSlice("mount")
	if err != nil {
		return caddy.ExitCodeFailedStartup, err
	}
	var mounts []HandlerMount
	for _, m := range mountFlags {
		dir, ro := strings.CutSuffix(m, ":ro")
		mounts = append(mounts, HandlerMount{Dir: dir, ReadOnly: ro})
	}

	route := caddyhttp.Route{
		HandlersRaw: []json.RawMessage{caddyconfig.JSONModuleObject(Handler{
			Root:        fl.String("root"),
			Mounts:      mounts,
			Router:      fl.String("router"),
			Concurrency: concurrency,
			INI:         ini,
			Env:         envs,
		}, "handler", "gophper", nil)},
	}
	if domain != "" {
		route.MatcherSetsRaw = []caddy.ModuleMap{{"host": caddyconfig.JSON(caddyhttp.MatchHost{domain}, nil)}}
	}
	if listen == "" {
		listen = ":80"
		if domain != "" {
			listen = ":" + strconv.Itoa(certmagic.HTTPSPort)
		}
	}
	server := &caddyhttp.Server{
		Listen:            []string{listen},
		ReadHeaderTimeout: caddy.Duration(10 * time.Second),
		IdleTimeout:       caddy.Duration(30 * time.Second),
		Routes:            caddyhttp.RouteList{route},
	}
	if fl.Bool("access-log") {
		server.Logs = &caddyhttp.ServerLogConfig{}
	}

	persist := false
	cfg := &caddy.Config{
		Admin: &caddy.AdminConfig{Disabled: true, Config: &caddy.ConfigSettings{Persist: &persist}},
		AppsRaw: caddy.ModuleMap{
			"http": caddyconfig.JSON(caddyhttp.App{Servers: map[string]*caddyhttp.Server{"php": server}}, nil),
		},
	}
	if err := caddy.Run(cfg); err != nil {
		return caddy.ExitCodeFailedStartup, err
	}
	select {}
}
