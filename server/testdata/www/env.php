<?php
header('Content-Type: application/json');
$env = [];
foreach (['TMPDIR', 'PHPRC', 'REDIRECT_STATUS', 'GOPHPER_CWD', 'PATH'] as $k) {
    $env[$k] = getenv($k) !== false || isset($_SERVER[$k]);
}
echo json_encode([
    'env' => $env,
    'temp_dir' => sys_get_temp_dir(),
    'ini_file' => php_ini_loaded_file(),
    'max_execution_time' => ini_get('max_execution_time'),
]);
