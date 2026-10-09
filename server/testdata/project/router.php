<?php
// A router for php -S: let static files through, answer the rest.
if (preg_match('/\.txt$/', $_SERVER['REQUEST_URI'])) {
    return false;
}
echo "router: ", $_SERVER['REQUEST_URI'], " script ", $_SERVER['SCRIPT_NAME'], " path info ", $_SERVER['PATH_INFO'] ?? '-', " cwd ", basename(getcwd()), "\n";
$leaked = array_filter(array_keys($_SERVER + getenv()), fn ($k) => str_starts_with($k, 'GOPHPER_')) || isset($router) || isset($vars);
echo "router env leaked: ", var_export($leaked, true), "\n";
