<?php

/*
 * Runs HTTPConfig.Router for gophper's HTTP server, as php -S runs a router
 * script. php-cgi cannot report a router's `return false`, so this script
 * runs instead and reports it with the X-Gophper-Router header.
 *
 * As with php -S, $_SERVER describes the script the request resolves to,
 * not the router. The router runs in the global scope, from the directory
 * the server started in. The variables that carry all this are removed
 * from $_SERVER and $_ENV first. With workers, getenv() still finds them:
 * it reads the FastCGI params, which putenv() cannot remove. They hold
 * only paths.
 */
// The closure cleans up and returns the router's path, so that require
// runs in the global scope with no variable of this script left over.
if ((require (static function (): string {
    $vars = [];
    foreach (['ROUTER', 'CWD', 'SCRIPT_FILENAME', 'SCRIPT_NAME', 'PHP_SELF', 'PATH_INFO'] as $name) {
        $k = 'GOPHPER_ROUTER_' . $name;
        $vars[$name] = $_SERVER[$k] ?? null;
        unset($_SERVER[$k], $_ENV[$k]);
        putenv($k);
    }
    foreach (['SCRIPT_FILENAME', 'SCRIPT_NAME', 'PHP_SELF', 'PATH_INFO'] as $name) {
        if ($vars[$name] === null || $vars[$name] === '') {
            unset($_SERVER[$name]);
        } else {
            $_SERVER[$name] = $vars[$name];
        }
    }
    chdir($vars['CWD']);
    return $vars['ROUTER'];
})()) === false && !headers_sent()) {
    header('X-Gophper-Router: pass');
}
