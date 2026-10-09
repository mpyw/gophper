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
| `gophper serve [options]` | Serves a PHP app over HTTP or HTTPS, with no web server in front |
| `gophper fcgi [options]` | A FastCGI server, like php-fpm |
| `gophper caddy [caddy arguments]` | Caddy with gophper built in. Automatic HTTPS. |
| `gophper extension list` / `install NAME...` | Lists or installs the extensions that come with gophper. See [Extensions](#extensions). |
| `gophper licenses` | Prints the licenses of gophper and everything in it |

> [!TIP]
> A symlink named `php` that points to `gophper` behaves like `gophper php`.

### Options for every command

Each option also reads an environment variable: `GOPHPER_` and its name in upper snake case.
For example, `--max-wait-time` reads `GOPHPER_MAX_WAIT_TIME`.

| Option | Meaning |
| --- | --- |
| `--extension-dir DIR` | Extensions that `extension=` and `dl()` load. See [Extensions](#extensions). |
| `--cache-dir DIR` | Where compiled code is kept between runs. Default: the user cache directory. |
| `--no-cache` | Compile the PHP binaries on every start |

These go before the subcommand: `gophper --extension-dir DIR serve`.

### CLI

```sh
gophper php -r 'echo PHP_VERSION, " ", PHP_OS, "\n";'
```

```
8.6.0RC3 WASI
```

The CLI mounts the host file system at `/`. It starts in the current directory.

| php.ini from | How |
| --- | --- |
| `/etc/gophper/php.ini` on the host | Read when it exists, as native PHP reads its own |
| A file | `gophper php -c FILE`, or `PHPRC=FILE` |
| A directory of `.ini` files | `PHP_INI_SCAN_DIR=DIR` |
| One setting | `gophper php -d KEY=VALUE` |

> [!TIP]
> `gophper php -S localhost:8000 router.php` runs PHP's built-in development server.
> It serves one request at a time, in a single instance. Use `gophper serve` for anything more.

### HTTP

```sh
gophper serve --root public --listen 127.0.0.1:8080
```

Workers serve the requests, as php-fpm's children do.
Each is a long-lived PHP instance that serves one request at a time, and PHP resets its state between them.
Requests are routed like Caddy's `php_server`:

| Request | Result |
| --- | --- |
| A file ending in `.php`, such as `/admin.php/users/1` | Runs it. The rest of the path is `PATH_INFO`. |
| Another existing file | Sent as is |
| A directory | Runs its index file |
| Anything else | Runs the front controller, `index.php` |
| A path with a segment that starts with `.`, such as `/.env` | 404, except `/.well-known` |

| Option | Meaning |
| --- | --- |
| `--listen ADDR` | Default: `127.0.0.1:8080`, or `:443` with `--domain` |
| `--root DIR` | The document root. It must be inside a `--mount`. Default: `.` |
| `--router FILE` | Runs for every request, as with `php -S`. If it returns `false`, the file is sent as is. |
| `--index FILE` | A file a directory runs. Repeatable. Default: `index.php` |
| `--front-controller FILE` | Runs for paths that are not files. `off` answers 404. Default: `index.php` |
| `--split-path SUFFIX` | Ends a script's path, before `PATH_INFO`. Repeatable. Default: `.php` |
| `--no-static` | Answers files other than scripts with 404 |
| `--max-body SIZE` | Limits request bodies, such as `64M`. `0` means no limit. Default: `64M`. A body is read in full before PHP runs, as nginx does. Beyond 1 MiB, it waits in a file in the system's temporary directory. With `0`, that file can grow without limit. |
| `--domain NAME` | Serves HTTPS with a Let's Encrypt certificate. Also listens on `:80` for the challenge. |
| `--tls-cert FILE`, `--tls-key FILE` | Serves HTTPS with these files |

For a Laravel-style layout, run from the project directory:

```sh
cd my-app
gophper serve --root public
```

`--mount` defaults to the current directory, so PHP reaches `vendor/` and `storage/` outside `public/`.

> [!WARNING]
> Laravel itself is not verified to boot yet.

### FastCGI

```sh
gophper fcgi --listen unix:/run/gophper.sock --mount /var/www -d max_execution_time=30
```

The web server chooses the script, as with php-fpm.

| Option | Meaning | php-fpm equivalent |
| --- | --- | --- |
| `--listen ADDR` | TCP address, or `unix:/path/to.sock`. Default: `127.0.0.1:9000` | `listen` |
| `--listen-mode MODE` | Permissions of a Unix socket. Default: `0660` | `listen.mode` |
| `--allowed-clients ADDR` | An address or prefix that may connect over TCP. Repeatable. Default: any | `listen.allowed_clients` |
| `--limit-extensions EXT` | An extension a script may have. Repeatable. Default: `.php` and `.phar` | `security.limit_extensions` |
| `--ping-path PATH` | Answers `pong` | `ping.path` |
| `--status-path PATH` | Answers counters as text | `pm.status_path` |

```nginx
location ~ \.php(/|$) {
    fastcgi_split_path_info ^(.+\.php)(/.*)$;
    fastcgi_pass unix:/run/gophper.sock;
    include fastcgi_params;
    fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
    fastcgi_param PATH_INFO $fastcgi_path_info;
}
```

> [!NOTE]
> Every FastCGI param reaches `$_SERVER` unchanged, including `SCRIPT_NAME` and `PATH_INFO`.
> `SCRIPT_FILENAME` must be inside a `--mount`.

### Options for serve and fcgi

| Option | Meaning | php-fpm equivalent |
| --- | --- | --- |
| `--mount DIR[:ro]` | A directory PHP may access, at the same path. Repeatable. Default: the current directory | |
| `--temp-dir DIR` | Mounted at `/tmp` inside PHP. Default: the system's | |
| `--no-processes` | Stops PHP from starting host programs (`proc_open`, `exec` and the rest) | `disable_functions` |
| `--no-workers` | Starts a fresh PHP instance for each request | |
| `--max-requests N` | Requests a worker serves before it is replaced. Default: 500 | `pm.max_requests` |
| `--opcache-dir DIR` | Where opcache keeps compiled scripts between requests. Default: the user cache directory | |
| `--no-opcache` | Leaves opcache off | `opcache.enable=0` |
| `--concurrency N` | PHP instances at once. Default: the number of CPUs | `pm.max_children` |
| `--max-wait-time DURATION` | How long a request waits for a free instance before 503. Default: no limit | |
| `-c FILE` | A php.ini file. `-d` entries come after it. | |
| `-d KEY=VALUE` | A php.ini entry. Repeatable. | `php_admin_value` |
| `-e KEY=VALUE` | An environment variable for PHP. Repeatable. | `env[KEY]` |
| `--access-log FILE` | Appends an access log. `-` means stdout. | `access.log` |

> [!IMPORTANT]
> PHP sees only `--mount` directories, `/tmp` and its own php.ini.
> The host environment is not passed to PHP, as with php-fpm's `clear_env`.
> The default php.ini sets `variables_order=GPCS`, as `php.ini-production` does.

> [!WARNING]
> A child process runs outside the mounts, with the rights of the server.
> PHP may start one by default, as with php-fpm. Use `--no-processes` to stop it.

### Caddy

`gophper caddy` is Caddy with the standard modules and gophper.
It takes the same arguments as the `caddy` command.

```sh
gophper caddy php-server --root ./public --domain example.com
gophper caddy run --config Caddyfile
```

For a full config, use the `gophper` directive in a Caddyfile:

```caddyfile
example.com {
	encode gzip
	gophper {
		root ./public
		mount . ro
		mount ./storage
		concurrency 8
		php_ini max_execution_time 30
		env APP_ENV production
	}
}
```

| Subdirective | Same as |
| --- | --- |
| `root`, `router`, `index`, `split_path`, `temp_dir`, `concurrency`, `max_wait_time`, `env` | The `serve` options |
| `processes off` | `--no-processes` |
| `opcache DIR \| off` | `--opcache-dir`, or `--no-opcache` |
| `workers off` | `--no-workers` |
| `max_requests N` | `--max-requests` |
| `mount DIR [ro]` | `--mount DIR[:ro]` |
| `front_controller FILE \| off` | `--front-controller` |
| `file_server off` | `--no-static`, as in FrankenPHP |
| `max_body SIZE \| off` | `--max-body` |
| `php_ini_file FILE` | `-c`. A relative path is from Caddy's working directory, as for `root`. |
| `php_ini KEY VALUE` | `-d` |

To build your own Caddy, import `github.com/mpyw/gophper/caddy`. It registers the `http.handlers.gophper` module.

### Extensions

Extensions load at runtime, as `.so` files do in native PHP.
Each one is a WebAssembly module, so one file runs on every platform.

gophper comes with some extensions. `gophper extension install` writes them to the extension directory:

```sh
gophper --extension-dir ./extensions extension install dl_test
gophper --extension-dir ./extensions php -d extension=dl_test -r 'echo dl_test_test2("ext"), "\n";'
```

```
extensions/dl_test.so
Hello ext
```

| Command | What it does |
| --- | --- |
| `gophper extension list` | Lists the extensions that come with gophper |
| `gophper --extension-dir DIR extension install NAME...` | Writes them to `DIR` as `NAME.so` |

| Way to set the directory | Example |
| --- | --- |
| Flag, before the subcommand | `gophper --extension-dir DIR serve` |
| Environment | `GOPHPER_EXTENSION_DIR=DIR` |
| Go | `EngineConfig.ExtensionDir` |

`extension=<name>` in php.ini, `-d extension=<name>`, and `dl("<name>.so")` all work.

Your own extensions go in the same directory.

> [!IMPORTANT]
> An extension must be a wasm side module, built with gophper-wasm's `scripts/build-ext.sh`.
> A native `.so` does not load.
> Build it against the same php-src version and configuration as gophper's PHP.

These come with gophper:

| Extension | What it adds |
| --- | --- |
| `gd` | Images, with PNG and JPEG |
| `intl` | ICU 78, with English and Japanese. `extension install intl` also writes ICU's data, 13 MB, beside it. |
| `bz2` | bzip2 |
| `gmp` | GMP, under the LGPL. See [License](#license). |
| `redis` | phpredis 6.3, with `session.save_handler=redis` |
| `sodium` | libsodium |
| `zip` | `ZipArchive`, with AES encryption |
| `dl_test` | php-src's extension for testing `dl()` |

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
| `github.com/mpyw/gophper/server` | `HTTPHandler`, an `http.Handler`, and `FastCGIServer`. Both take a `PHPConfig`. |
| `github.com/mpyw/gophper/caddy` | The Caddy module. Only a program that imports it links Caddy. |

PHP can call functions written in Go:

```go
code, err := engine.RunCLI(ctx, gophper.Options{
	Args:   []string{"-r", `echo go_add(20, 22), "\n";`},
	Stdout: os.Stdout,
	Functions: map[string]gophper.Function{
		"go_add": func(ctx context.Context, args []any) (any, error) {
			return args[0].(int64) + args[1].(int64), nil
		},
	},
})
```

| PHP | Go |
| --- | --- |
| `null`, `bool`, `int`, `float`, `string` | `nil`, `bool`, `int64`, `float64`, `string` |
| An array with the keys 0 to n-1 | `[]any` |
| Any other array, or an object | `map[string]any` |
| An exception (`RuntimeException`) | A returned `error` |

PHP gets nothing of the host unless `Options` says so:

| Field | Meaning | Zero value |
| --- | --- | --- |
| `FS` | The directories PHP sees | No file system |
| `HostPath` | Maps a path inside PHP to its host file, for permissions, owners, locks and child processes | No path has a host file |
| `Processes` | PHP may start host programs | `proc_open` and the rest fail |
| `Signals` | Signals for PHP, as from `signal.Notify`. Handled ones run the `pcntl` handler. The rest end the run with exit code 128 plus the signal number. | No signals |

`DefaultEngineConfig` keeps a per-user cache directory: wazero's compiled code, and the PHP binaries decompressed.
With the cache, `gophper php -r 'echo 1;'` takes about 0.18 seconds.
Most of it is wazero validating the 16 MB binary, which it does even with the cache.

## What works

| Feature | Status |
| --- | --- |
| Language (enums, generators, property hooks, pipe operator, ...) | Works |
| Fatal errors, `exit()`, shutdown functions | Works |
| `max_execution_time`, `set_time_limit()` | Works. The timer runs in Go, since WASI has no signals. |
| `pcntl`: signals, `pcntl_alarm()`, `pcntl_waitpid()` | Works. `gophper php` passes `SIGINT`, `SIGTERM`, `SIGHUP`, `SIGUSR1` and others to PHP, so `artisan queue:work` stops cleanly. `pcntl_fork()` fails. `pcntl_exec()` runs the program as a child and exits with its status. |
| opcache | In `serve`, `fcgi` and Caddy, each worker keeps compiled scripts in its own shared memory, 64 MB. A file cache keeps them between workers, and for fresh instances. Raise the size with `-d opcache.memory_consumption=128`. The CLI leaves opcache off, as native PHP does. |
| Sockets | Tested: TCP clients and servers, UDP and Unix clients, `stream_select`, and the `http://` wrapper |
| DNS | Works, through Go's resolver |
| `date`, `pcre`, `hash`, `json`, `random`, `spl`, `uri`, `lexbor` | Works |
| `openssl`, including HTTPS with certificate checks | Works. The CA bundle is Mozilla's, from gophper-wasm. |
| `dom`, `xml`, `simplexml`, `xmlreader`, `xmlwriter` | Works |
| PDO with `pdo_mysql`, `pdo_pgsql` and `pdo_sqlite` | Works. Tested over TCP, Unix sockets and TLS, with MySQL's `caching_sha2_password` and PostgreSQL's SCRAM. |
| `sqlite3`, `mysqli`, `zlib` | Works |
| Fibers | Throws `Fibers are not supported on this platform`. |
| `proc_open`, `exec`, `shell_exec`, `system`, `passthru`, `popen` | Works. Children are host processes, started with Go's `os/exec`. Pipes, `socket` descriptors, files, the environment and the working directory are passed. |
| `PHP_BINARY` | Runs `gophper php` again, with the same global options. Composer and `artisan` start PHP this way. |
| `posix_*`, users and groups | Works. They are the host's: the uid gophper runs as, and its user database. |
| `flock`, `chmod`, `chown`, `fileperms`, `fileowner`, `is_executable` | Works, on the host file. WASI itself has no permissions or locks. |
| `dns_get_record`, `checkdnsrr`, `getmxrr` | Works. The host queries the name servers in `/etc/resolv.conf`. |
| Other built-in extensions | bcmath, calendar, ctype, exif, fileinfo, filter, iconv, mbstring, Phar, posix, session, tokenizer |
| Loading extensions at runtime | Works. See [Extensions](#extensions). |
| `curl`, `pgsql` | Built in. curl uses the same OpenSSL and CA bundle. |
| `bz2`, `gd`, `gmp`, `intl`, `redis`, `sodium`, `zip` | Load at runtime. See [Extensions](#extensions). |

> [!NOTE]
> A script blocked reading a socket is not stopped by `max_execution_time`.
> Native PHP on Linux behaves the same, since it counts CPU time.

## Performance

On an Apple Silicon Mac:

| Benchmark | Native PHP 8.5 (`php -n`) | gophper |
| --- | --- | --- |
| `testdata/bench.php` | 197 ms | 656 ms |

gophper is about 3.3 times slower. There is no JIT.

A small JSON page with `gophper serve`, measured with `ab`:

| Mode | One request at a time | 8 at once, 4 instances |
| --- | --- | --- |
| Workers (the default) | 0.6 ms | 8,700 requests per second |
| A fresh instance per request (`--no-workers`) | 12 ms | 290 requests per second |

Laravel 13's welcome page with `gophper serve`, one request at a time:

| Mode | Time per request |
| --- | --- |
| Workers, with opcache's shared memory and file cache (the default) | 23 ms |
| Fresh instances, with the file cache | 100 ms |
| Fresh instances, without opcache | 160 ms |

## License

gophper's own code is under the [MIT License](LICENSE).

A gophper binary also contains PHP and the libraries built into it, from gophper-wasm, and Go modules.
Each keeps its own license. `gophper licenses` prints them all, with the license of each Go module linked in.

> [!IMPORTANT]
> To distribute a gophper binary, pass on what `gophper licenses` prints.
> `gmp.so` links GMP under the LGPL. gophper-wasm's README says what that asks of a distributor.
