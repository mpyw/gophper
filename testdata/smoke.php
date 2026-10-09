<?php
declare(strict_types=1);

// PHP 8.4: property hooks / asymmetric visibility
final class Temperature {
    public private(set) float $celsius = 0.0;
    public float $fahrenheit {
        get => $this->celsius * 9 / 5 + 32;
        set (float $v) { $this->celsius = ($v - 32) * 5 / 9; }
    }
}
$t = new Temperature();
$t->fahrenheit = 212.0;
printf("hooks: %.1f C\n", $t->celsius);

// PHP 8.5: pipe operator, array_first/array_last
$r = "  Hello Gophper  " |> trim(...) |> strtoupper(...) |> str_split(...) |> array_first(...);
echo "pipe: $r ", array_last([1, 2, 3]), "\n";

enum Suit: string { case Hearts = 'H'; case Spades = 'S'; }
echo "enum: ", Suit::from('S')->name, "\n";

function gen() { foreach (range(1, 3) as $i) yield $i => $i * $i; }
echo "generator: ", json_encode(iterator_to_array(gen())), "\n";

try {
    intdiv(1, 0);
} catch (DivisionByZeroError $e) {
    echo "exception: ", $e->getMessage(), "\n";
} finally {
    echo "finally: ok\n";
}

try {
    $f = new Fiber(fn () => Fiber::suspend(1));
    $f->start();
} catch (Throwable $e) {
    echo "fiber: ", get_class($e), ": ", $e->getMessage(), "\n";
}

echo "preg: ", preg_replace('/(\w+) (\w+)/u', '$2 $1', 'hello wörld'), "\n";
echo "json: ", json_encode(['a' => 1, 'b' => [true, null, 1.5]], JSON_THROW_ON_ERROR), "\n";
date_default_timezone_set('Asia/Tokyo');
echo "date: ", (new DateTimeImmutable('2026-10-09 12:00:00 UTC'))->setTimezone(new DateTimeZone('Asia/Tokyo'))->format(DATE_ATOM), "\n";
echo "hash: ", hash('sha256', 'gophper'), "\n";
echo "password: ", password_verify('secret', password_hash('secret', PASSWORD_BCRYPT)) ? 'ok' : 'ng', "\n";
echo "random: ", strlen(random_bytes(16)), " bytes, ", (new Random\Randomizer())->getInt(1, 1), "\n";

$tmp = sys_get_temp_dir() . '/gophper-' . bin2hex(random_bytes(4)) . '.txt';
file_put_contents($tmp, "written by wasm\n");
echo "file: ", trim(file_get_contents($tmp)), " (", filesize($tmp), " bytes)\n";
unlink($tmp);
echo "dir: ", count(scandir(__DIR__)) > 2 ? 'ok' : 'ng', "\n";

$uri = Uri\Rfc3986\Uri::parse('https://example.com/a/../b?q=1');
echo "uri: ", $uri->toString(), "\n";

echo "stdin: ", trim(fgets(STDIN) ?: '(none)'), "\n";
echo "exts: ", implode(',', get_loaded_extensions()), "\n";
