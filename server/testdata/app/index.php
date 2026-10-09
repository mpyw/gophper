<?php
// Front controller: everything that is not a file ends up here.
$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
if ($path === '/redirect') {
    header('Location: /target');
    exit;
}
if ($path === '/created') {
    http_response_code(201);
    header('X-Custom: yes');
    echo "created\n";
    exit;
}
if ($path === '/post') {
    echo json_encode(['post' => $_POST, 'cookie' => $_COOKIE, 'raw' => file_get_contents('php://input')]);
    exit;
}
header('Content-Type: application/json');
echo json_encode([
    'script_name' => $_SERVER['SCRIPT_NAME'],
    'script_filename' => basename($_SERVER['SCRIPT_FILENAME']),
    'path_info' => $_SERVER['PATH_INFO'] ?? '',
    'request_uri' => $_SERVER['REQUEST_URI'],
    'query' => $_GET,
    'https' => $_SERVER['HTTPS'] ?? '',
    'host' => $_SERVER['HTTP_HOST'],
    'software' => $_SERVER['SERVER_SOFTWARE'],
]);
