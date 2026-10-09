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
if ($path === '/bad-status') {
    header('Status: 42 Nonsense');
    exit;
}
if ($path === '/location-200') {
    // A Location with the status put back to 200: php-cgi then sends no
    // Status, and the server makes it a redirect, as CGI does.
    header('Location: /target');
    http_response_code(200);
    exit;
}
if ($path === '/text-status') {
    // Not a number: the status stays 200.
    header('Status: none');
    exit;
}
if ($path === '/space-status') {
    // Only a no-break space, which Go counts as space and textproto keeps: no
    // status at all, which is a 502.
    header("Status: \u{a0}");
    exit;
}
if ($path === '/real-ip') {
    echo $_SERVER['HTTP_X_REAL_IP'] ?? 'none', "\n";
    echo $_SERVER['HTTP_PROXY'] ?? 'no proxy', "\n";
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
