#!/usr/bin/env bash
# Runs a released gophper binary the way a user would: `gophper php` with
# -r and with script files, and `gophper serve`. The scripts use namespaces
# and an autoloader that maps them to paths, as Composer's does.
#
# Usage: release-smoke.sh GOPHPER_BINARY EXPECTED_VERSION
set -euo pipefail

bin=$1
version=${2#v}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Windows' PHP may end lines with \r\n. The tests compare the text only.
run() { "$bin" "$@" | tr -d '\r'; }

expect() {
  local name=$1 want=$2 got=$3
  if [[ "$got" != "$want" ]]; then
    printf '::error::%s\nwant: %q\ngot:  %q\n' "$name" "$want" "$got"
    exit 1
  fi
  echo "ok: $name"
}

# A native path: C:\... on Windows (Git Bash), /... elsewhere.
native() { if command -v cygpath >/dev/null; then cygpath -w "$1"; else echo "$1"; fi; }

expect "--version" "$version" "$(run --version | grep -o "$version" | head -n1)"
expect "php -r" "hello" "$(run php -r 'echo "hello";')"

mkdir -p "$work/app/src/Util" "$work/app/public"
cat >"$work/app/autoload.php" <<'PHP'
<?php
spl_autoload_register(function (string $class): void {
    $prefix = 'App\\';
    if (strncmp($class, $prefix, strlen($prefix)) !== 0) {
        return;
    }
    $file = __DIR__ . '/src/' . str_replace('\\', '/', substr($class, strlen($prefix))) . '.php';
    if (is_file($file)) {
        require $file;
    }
});
PHP
cat >"$work/app/src/Util/Greeter.php" <<'PHP'
<?php
namespace App\Util;

final class Greeter
{
    public function greet(string $name): string
    {
        return sprintf('hello %s from %s', $name, static::class);
    }
}
PHP
cat >"$work/app/main.php" <<'PHP'
<?php
namespace App;

use App\Util\Greeter;

require __DIR__ . '/autoload.php';

echo (new Greeter())->greet('cli');
PHP
cat >"$work/app/public/index.php" <<'PHP'
<?php
namespace App;

use App\Util\Greeter;

require dirname(__DIR__) . '/autoload.php';

echo (new Greeter())->greet('serve');
PHP

want_cli='hello cli from App\Util\Greeter'
expect "php relative script" "$want_cli" "$(cd "$work/app" && run php main.php)"
expect "php absolute script" "$want_cli" "$(run php "$(native "$work/app/main.php")")"

# serve on a free port, read from its "serving ... on http://ADDR" line.
# The first run compiles PHP, which takes a while on a runner.
log=$work/serve.log
(cd "$work/app" && exec "$bin" serve --root public --listen 127.0.0.1:0) 2>"$log" &
pid=$!
# Wait for serve to exit first: Windows cannot remove its working directory
# while it runs. A leftover temporary directory fails no test.
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; rm -rf "$work" || true' EXIT
addr=
for _ in $(seq 1 180); do
  addr=$(tr -d '\r' <"$log" | sed -nE 's#.*serving .* on http://([^ ]+).*#\1#p' | head -n1)
  [[ -n "$addr" ]] && break
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "::error::gophper serve exited before it listened"
    cat "$log"
    exit 1
  fi
  sleep 1
done
if [[ -z "$addr" ]]; then
  echo "::error::gophper serve did not listen"
  cat "$log"
  exit 1
fi
expect "serve" 'hello serve from App\Util\Greeter' "$(curl -fsS "http://$addr/" | tr -d '\r')"
