# gophper

Run PHP 8.6 from Go with no cgo.

gophper runs php-src, compiled to WebAssembly, on [wazero](https://github.com/tetratelabs/wazero).
It is the real Zend Engine, so the language behaves exactly like PHP.

> [!WARNING]
> This is an experiment. The API will change.

## Install

Only Go is needed.

```sh
CGO_ENABLED=0 go install github.com/mpyw/gophper/cmd/gophper@latest
```

The PHP binaries come from [gophper-wasm](https://github.com/mpyw/gophper-wasm), a Go module that embeds them.

## Usage

| Command | What it does |
| --- | --- |
| `gophper php [php options] [file] [args...]` | The php CLI. Every argument goes to PHP. |
| `gophper serve [options]` | Serves a PHP app over HTTP or HTTPS, with no web server |
| `gophper fcgi [options]` | A FastCGI server, like php-fpm |
| `gophper-caddy php-server [options]` | Caddy with gophper built in. Automatic HTTPS. |

> [!TIP]
> A symlink named `php` that points to `gophper` behaves like `gophper php`.

### CLI

```sh
gophper php -r 'echo PHP_VERSION, " ", PHP_OS, "\n";'
```

```
8.6.0RC3 WASI
```

The CLI mounts the host file system at `/`. It starts in the current directory.

### HTTP

```sh
gophper serve --root ./public --listen 127.0.0.1:8080
```

Requests are routed like Caddy's `php_server`:

| Request | Result |
| --- | --- |
| An existing `.php` file, such as `/admin.php/users/1` | Runs it. The rest of the path is `PATH_INFO`. |
| Another existing file | Sent as is |
| A directory with `index.php` | Runs that `index.php` |
| Anything else | Runs the root's `index.php`, the front controller |

| Option | Meaning |
| --- | --- |
| `--listen` | TCP address |
| `--root` | The document root. PHP can access only this directory. |
| `--workers` | Max concurrent PHP instances. `0` means the number of CPUs. |
| `-d key=value` | A php.ini entry. Repeatable. |
| `--tls-cert`, `--tls-key` | Serve HTTPS with these files |

### FastCGI

```sh
gophper fcgi --listen 127.0.0.1:9000 --root /var/www -d max_execution_time=30
```

It takes `--root`, `--workers` and `-d` as `serve` does. `--listen` also accepts `unix:/path/to.sock`.
Point the web server at it as you would at php-fpm.

```nginx
location ~ \.php(/|$) {
    fastcgi_split_path_info ^(.+\.php)(/.*)$;
    fastcgi_pass 127.0.0.1:9000;
    include fastcgi_params;
    fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
    fastcgi_param PATH_INFO $fastcgi_path_info;
}
```

> [!NOTE]
> Every FastCGI param reaches `$_SERVER` unchanged, including `SCRIPT_NAME` and `PATH_INFO`.
> Nothing else is added. The host environment is not passed to PHP.

### Caddy

`gophper-caddy` is Caddy with the standard modules and gophper.

```sh
go install github.com/mpyw/gophper/caddy/cmd/gophper-caddy@latest
gophper-caddy php-server --root ./public --domain example.com
```

`--domain` listens on the HTTPS port and gets a certificate automatically.
For a full config, use the `gophper` directive in a Caddyfile:

```caddyfile
example.com {
	encode gzip
	gophper {
		root ./public
		workers 8
		ini max_execution_time 30
	}
}
```

```sh
gophper-caddy run --config Caddyfile
```

To build your own Caddy, import `github.com/mpyw/gophper/caddy`. It registers the `http.handlers.gophper` module.

### Extensions

Extensions load at runtime, as `.so` files do in native PHP.
Each one is a WebAssembly module built by gophper-wasm, so one file runs on every platform.

```sh
gophper --extension-dir ./extensions php -d extension=dl_test -r 'echo dl_test_test2("ext"), "\n";'
```

```
Hello ext
```

| Way to set the directory | Example |
| --- | --- |
| Flag, before the subcommand | `gophper --extension-dir DIR serve` |
| Environment | `GOPHPER_EXTENSION_DIR=DIR` |
| Go | `EngineConfig.ExtensionDir` |

`extension=<name>` in php.ini, `-d extension=<name>`, and `dl("<name>.so")` all work.

> [!NOTE]
> Only `dl_test`, php-src's test extension, is published so far.

### From Go

```go
engine, err := gophper.NewEngine(ctx, gophper.DefaultEngineConfig())
if err != nil {
	return err
}
defer engine.Close(ctx)

code, err := engine.RunCLI(ctx, gophper.Options{
	Args:   []string{"-r", "echo 1 + 1;"},
	Stdout: os.Stdout,
	Stderr: os.Stderr,
})
```

| Package | Contents |
| --- | --- |
| `github.com/mpyw/gophper` | `Engine`: runs the CLI or CGI SAPI once per call |
| `github.com/mpyw/gophper/server` | `HTTPHandler`, an `http.Handler`, and `FastCGIServer` |
| `github.com/mpyw/gophper/caddy` | The Caddy module. A separate Go module, so the core does not depend on Caddy. |

`DefaultEngineConfig` keeps wazero's compiled code in a per-user cache directory.
With the cache, `gophper php -r 'echo 1;'` takes about 0.07 seconds.

## What works

| Feature | Status |
| --- | --- |
| Language (enums, generators, property hooks, pipe operator, ...) | Works |
| Fatal errors, `exit()`, shutdown functions | Works |
| `max_execution_time`, `set_time_limit()` | Works. The timer runs in Go, since WASI has no signals. |
| Sockets | Tested: TCP clients and servers, UDP and Unix clients, `stream_select`, and the `http://` wrapper |
| DNS | Works, through Go's resolver |
| `date`, `pcre`, `hash`, `json`, `random`, `spl`, `uri`, `lexbor` | Works |
| Fibers | Throws `Fibers are not supported on this platform`. |
| `proc_open`, `exec`, `posix_*` | Not yet |
| Built-in extensions | bcmath, calendar, ctype, exif, fileinfo, filter, iconv, mbstring, mysqli, PDO, pdo_mysql, Phar, posix, session, tokenizer, and the core ones |
| Loading extensions at runtime | Works. See [Extensions](#extensions). |
| Other extensions (`openssl`, `curl`, `intl`, `gd`, `pdo_sqlite`, ...) | Not built yet |

> [!NOTE]
> A script blocked reading a socket is not stopped by `max_execution_time`.
> Native PHP on Linux behaves the same, since it counts CPU time.

## Performance

On an Apple Silicon Mac:

| Benchmark | Native PHP 8.5 (`php -n`) | gophper |
| --- | --- | --- |
| `testdata/bench.php` | 197 ms | 656 ms |

gophper is about 3.3 times slower. There is no JIT.

FastCGI, for a small JSON page with 4 workers:

| Load | Time per request |
| --- | --- |
| One request at a time, keep-alive | 3.3 ms |
| Parallel | 1.1 ms (about 930 requests per second) |

Each request runs in a fresh PHP instance.
