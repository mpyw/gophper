---
name: performance
description: Measure gophper's speed, or change a number in README's Performance section. Use before claiming something is faster or slower, after a wazero or gophper-wasm upgrade, or when changing workers, opcache or the pool.
---

# Measure performance

README's numbers are measured, never estimated. Run each again after a change that could move it.

| Number | How |
| --- | --- |
| `testdata/bench.php` | `go build -o /tmp/g ./cmd/gophper && /tmp/g php testdata/bench.php`, and native `php -n testdata/bench.php` |
| A small page | `gophper serve` on a one-line JSON script, then `ab -q -n 2000 -c 8 http://127.0.0.1:PORT/` after a warm-up of 200 |
| Laravel's welcome page | `composer create-project laravel/laravel build/laravel` once (`build/` is ignored). `gophper serve --root public` from there, then `ab -n 200 -c 1` after 30 warm-up requests. |

## Rules

- Warm up first. The first requests compile scripts into opcache, and the first instance compiles the wasm.
- Give `--opcache-dir` a fresh directory to measure a cold file cache.
- Measure one request at a time (`-c 1`) for latency, and several for throughput.
- Watch memory too. Each worker has its own opcache shared memory: 4 workers with 128 MB each reached 1.96 GB RSS.
- Say what machine the numbers came from, as README does.
