<?php
$t = hrtime(true);
function fib($n) { return $n < 2 ? $n : fib($n - 1) + fib($n - 2); }
fib(27);
$a = [];
for ($i = 0; $i < 300000; $i++) { $a["k$i"] = $i * 2; }
$s = 0; foreach ($a as $k => $v) { $s += strlen($k) + $v; }
$str = str_repeat("lorem ipsum dolor ", 20000);
for ($i = 0; $i < 20; $i++) { $str = preg_replace('/o(r|l)/', 'O$1', $str); }
usort($a, fn ($x, $y) => $y <=> $x);
printf("%.0f ms\n", (hrtime(true) - $t) / 1e6);
