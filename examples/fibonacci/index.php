<?php

header('Content-Type: text/plain; charset=utf-8');

function php_fib(int $n): int
{
    return $n < 2 ? $n : php_fib($n - 1) + php_fib($n - 2);
}

/** Runs $fib($n) and returns its result with the milliseconds it took. */
function measure(callable $fib, int $n): array
{
    $start = hrtime(true);
    $result = $fib($n);

    return ['result' => $result, 'ms' => round((hrtime(true) - $start) / 1e6, 3)];
}

// PHP's int is 32-bit on wasm32, so F(46) is the largest that fits.
$n = max(0, min(35, (int) ($_GET['n'] ?? 27)));

$php = measure(php_fib(...), $n);
$go = measure(go_fib(...), $n);

var_dump([
    'n' => $n,
    'php' => $php,
    'go' => $go,
    'same' => $php['result'] === $go['result'],
    'go_faster_by' => $go['ms'] > 0 ? round($php['ms'] / $go['ms'], 1) : INF,
]);
