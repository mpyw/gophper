# gophper examples

A Go module of its own, so that the root module does not depend on anything here.
It uses the released `github.com/mpyw/gophper`.
Its path, `example/gophper`, is outside `github.com/mpyw/gophper`, so it cannot import gophper's `internal/` packages.
Run each command from this directory.

| Example | What it shows |
| --- | --- |
| [`fibonacci`](fibonacci) | A Go function that PHP calls, against the same recursion in PHP |
| [`todos`](todos) | A Laravel app with Blade only, and no JavaScript build. `todos-serve` and `todos-fcgi` run it. |
| [`todos-serve`](todos-serve) | `todos` over HTTP, with `server.HTTPHandler`, as `gophper serve` does |
| [`todos-fcgi`](todos-fcgi) | `todos` as a FastCGI server, with FrankenPHP in front as the web server |

## fibonacci

```sh
go run ./fibonacci
curl 'http://127.0.0.1:8080/?n=30'
```

`index.php` computes F(n) by naive recursion twice: in PHP, and in Go through `go_fib()`.
It prints both with `var_dump`, as `text/plain`.
`n` goes up to 35.

```
array(5) {
  ["n"]=>
  int(30)
  ["php"]=>
  array(2) {
    ["result"]=>
    int(832040)
    ["ms"]=>
    float(253.794)
  }
  ["go"]=>
  array(2) {
    ["result"]=>
    int(832040)
    ["ms"]=>
    float(2.707)
  }
  ["same"]=>
  bool(true)
  ["go_faster_by"]=>
  float(93.8)
}
```

The numbers are from an Apple Silicon Mac.
Both sides run the same algorithm, so the gap is wazero's PHP against Go's machine code.

> [!NOTE]
> `server.HTTPHandler` has no Go functions, so `main.go` runs `Engine.RunCGI` for each request itself.
> `index.php` is embedded and mounted at `/app`.

## todos

Set it up once. Composer and `artisan` run on gophper too, or on a native PHP 8.3 or later:

```sh
cd todos
composer install
cp .env.example .env
gophper php artisan key:generate
gophper php artisan migrate
cd ..
```

The todos are in SQLite, in `todos/database/database.sqlite`.
`gophper php artisan test` runs the tests.

## todos-serve

```sh
go run ./todos-serve
```

Open http://127.0.0.1:8080/.
PHP sees the whole of `todos/`, so it reaches `vendor/` and `.env`.
Only `todos/public/` is served.

## todos-fcgi

FrankenPHP serves HTTP and passes PHP requests to gophper over FastCGI, as it would to php-fpm.
FrankenPHP's own PHP does not run.
[mise](https://mise.jdx.dev/) installs FrankenPHP from `todos-fcgi/mise.toml`.

```sh
go run ./todos-fcgi                  # FastCGI on 127.0.0.1:9000
(cd todos-fcgi && mise run web)      # In another terminal: HTTP on 127.0.0.1:8080
```

Open http://127.0.0.1:8080/.

| File | Role |
| --- | --- |
| `main.go` | The FastCGI server, with `server.FastCGIServer` |
| `Caddyfile` | `php_fastcgi 127.0.0.1:9000`, with `root` at `todos/public` |
| `mise.toml` | FrankenPHP, `TODOS_ROOT` for the `Caddyfile`, and the `web` task |

> [!IMPORTANT]
> The `root` must be an absolute path.
> Caddy sends it in `SCRIPT_FILENAME`, and gophper runs only scripts inside its mounts.
