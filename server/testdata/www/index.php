<?php
$mode = $_GET['mode'] ?? 'info';

if (isset($_GET['shutdown'])) {
    register_shutdown_function(fn () => print("shutdown ran\n"));
}
if ($mode === 'spin') {
    for (;;) {}
}
if ($mode === 'sleep') {
    usleep((int) $_GET['ms'] * 1000);
    echo "slept\n";
    exit;
}
if ($mode === 'fatal') {
    undefined_fn();
}

http_response_code(201);
header('X-Gophper: yes');
header('Content-Type: application/json');
echo json_encode([
    'sapi' => PHP_SAPI,
    'script_name' => $_SERVER['SCRIPT_NAME'] ?? null,
    'path_info' => $_SERVER['PATH_INFO'] ?? null,
    'request_uri' => $_SERVER['REQUEST_URI'] ?? null,
    'method' => $_SERVER['REQUEST_METHOD'],
    'get' => $_GET,
    'post' => $_POST,
    'body_len' => strlen(file_get_contents('php://input')),
    'ua' => $_SERVER['HTTP_USER_AGENT'] ?? null,
    'custom' => $_SERVER['GOPHPER_CUSTOM'] ?? null,
]);
