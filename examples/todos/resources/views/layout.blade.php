<!DOCTYPE html>
<html lang="{{ str_replace('_', '-', app()->getLocale()) }}">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>@yield('title') - {{ config('app.name') }}</title>
    <style>
        :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
        body { max-width: 32rem; margin: 3rem auto; padding: 0 1rem; }
        form { margin: 0; }
        .add { display: flex; gap: .5rem; margin-bottom: 1.5rem; }
        .add input { flex: 1; padding: .5rem; font: inherit; }
        ul { list-style: none; padding: 0; }
        li { display: flex; align-items: center; gap: .5rem; padding: .5rem 0; border-bottom: 1px solid #8884; }
        li .title { flex: 1; }
        li.done .title { text-decoration: line-through; opacity: .6; }
        button { font: inherit; cursor: pointer; }
        .error { color: #d33; }
        footer { margin-top: 2rem; font-size: .8rem; opacity: .6; }
    </style>
</head>
<body>
    @yield('content')
    <footer>PHP {{ PHP_VERSION }} on {{ PHP_OS }}, Laravel {{ app()->version() }}</footer>
</body>
</html>
