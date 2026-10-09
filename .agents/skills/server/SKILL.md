---
name: server
description: Change gophper serve or gophper fcgi - routing, workers, the pool, opcache, php.ini handling, request bodies, TLS, mounts, or a new option. Use before editing server/, internal/fcgi, or serveAction, fcgiAction and their flags in cmd/gophper.
---

# serve and fcgi

| Piece | Where |
| --- | --- |
| Routing, static files, request bodies | `server/http.go` |
| FastCGI server, for nginx and others | `server/fastcgi.go`, `internal/fcgi/fcgi.go` |
| Instances, php.ini, opcache, mounts | `server/pool.go` |
| Workers: php-cgi with `-b` on a Unix socket | `server/worker.go`, `internal/fcgi/client.go` |
| php.ini files | `server/ini.go` |
| Flags, TLS, Let's Encrypt | `cmd/gophper/main.go` |

## Add an option

1. Add the field to `PHPConfig` (both servers), `HTTPConfig` or `FastCGIConfig`, with a doc comment.
2. Add the flag in `cmd/gophper/main.go` with `Sources: env("NAME")`, so that `GOPHPER_NAME` sets it too.
3. Add a row to README's option table, with the php-fpm or nginx setting it matches.
4. Test it through `startHTTPWith` or `startFCGIWith` in `server/*_test.go`.

## Rejected designs

| Idea | Why not |
| --- | --- |
| Environment variables for php-cgi (`PHPRC`, `TMPDIR`, `REDIRECT_STATUS`) | Scripts saw them. The php.ini at `/etc/gophper` replaces all three. |
| `net/http/fcgi` for the FastCGI server | It drops `SCRIPT_NAME` and `PATH_INFO`. `internal/fcgi` passes every param through. |
| php-cgi's FastCGI mode on fd 0 | php-cgi needs a listening socket there, which WASI cannot hand it. Workers start it with `-b` on a Unix socket instead, and compat's sockets listen on it. |
| Streaming a request body to PHP as it arrives | PHP reads all of `CONTENT_LENGTH` before it ends a request, so a client that trickled its body held a PHP instance. A few such clients made every other request wait. `httpBufferBody` reads the whole body first, as nginx does, keeping 1 MiB in memory and the rest in a temporary file. |
| One `--root` for both the document root and what PHP may access | Laravel's `public/index.php` reads `../vendor`. `--root` is only the document root, and `--mount` (default: the current directory) is what PHP reaches. |
| Serving every existing file | `.env` and `.git` leaked when the root was a project directory. Any path segment starting with `.` is 404, except `.well-known`, as in Laravel's nginx config. |
| Running a router as `SCRIPT_FILENAME` | php-cgi cannot report `return false`, and `$_SERVER` would describe the router. `server/bootstrap/router.php` runs it and restores what php -S would set. |
| Passing router details in variables and unsetting them | With PHP's built-in `variables_order=EGPCS`, `getenv()` returns a copy of `$_ENV` taken at startup, so `putenv()` cannot hide them. The default php.ini sets `GPCS`, as `php.ini-production` does. |
| FastCGI running any `SCRIPT_FILENAME` | A web server could pass an uploaded file. `LimitExtensions` defaults to `.php` and `.phar`, like php-fpm's `security.limit_extensions`. |
| Mapping guest paths to host paths by identity in `server` | `/tmp` is `TempDir` there, and a read-only mount must refuse `chmod`. `pool.hostPath` follows the mounts. |
| A path PHP writes and reads again, outside `HostPath` | `stat` then reports uid 0 and mode 0. opcache rejects a file cache entry it does not own. It compiled every script on every request, and `serve` got two times slower. `pool.hostPath` covers the opcache directory. |
| Keying opcache's file cache by opcache's system id alone | Two builds of one PHP release share it. `Engine.BuildID` hashes the binaries, and the pool's cache directory goes under it. |
| A fresh PHP instance for every request | Starting one takes 12 ms, more than most requests. Workers run php-cgi in FastCGI mode on a Unix socket, as php-fpm's children do, and `internal/fcgi.Do` sends them requests. `NoWorkers` keeps the old way. |
| php-cgi's own `PHP_FCGI_MAX_REQUESTS` | It closed the socket under a request that had already connected, which got a 502. The pool counts requests, sets `PHP_FCGI_MAX_REQUESTS=0`, and stops a worker itself. |
| Workers with opcache's file cache only | Laravel took 84 ms, and 23 ms with shared memory. An earlier try with shared memory was slower only because php.wasm had no SHM backend, and opcache recompiled every script. Shared memory is per worker, so `poolOpcacheWorkerINI` sets 64 MB: 128 MB put 4 workers at 1.96 GB RSS. |
| Using a web server's `SCRIPT_FILENAME` spelling as is | Windows matches paths without case, so `C:\App\x.php` passed the mount check for `C:\app`. Its guest path `/c/App/x.php` matched no wazero mount. `pool.mountSpelling` rewrites it with the mount's spelling. |
| Passing `X-Real_IP` on as `HTTP_X_REAL_IP` | It would pass for `X-Real-IP`, which a proxy in front sets. Header names with `_` are dropped, as nginx and Apache do. |
| Calling `WriteHeader` with whatever `Status:` PHP sent | net/http panics outside 100-999. A code below 200 or above 999 is a 502. |
| `strings.ToLower` to match SplitPath without case | It changes the length of invalid UTF-8 and of some letters, and `splitScript` indexes the original with its positions. `/%FF%FF.php` panicked. `httpLowerASCII` lowers A to Z only. |
| Sending the file as is when a router returns false | `/index.php` then sent PHP source. As `php -S` does, the script the path names runs, with the same body, which `readBody` reads once. |
| Taking 8.3 short names on Windows as plain names | `GIT~1` is `.git`, which `httpHidden` would not see. `httpUnsafe` rejects a tilde before a digit. |
| Clearing the FastCGI read deadline at `FCGI_BEGIN_REQUEST` | A client that sent nothing more held the connection forever. So did a kept-alive one after its first request, since the reader was already blocked without a deadline. The idle deadline holds until the params are in, and `handle` sets it again before `FCGI_END_REQUEST`. |
| `fcgi.Do` returning at `FCGI_END_REQUEST` while its writer still runs | The writer had one more read of stdin to make. After a router's `return false`, the next run reads the same body from the start, and the writer took part of it. `Do` stops its writer and waits for it before returning. |
| No network for PHP in `server` by default | php-fpm gives PHP the network, and apps need their database. `PHPConfig.NoNetwork` and `--no-network` turn it off, as `--no-processes` does for programs. |
