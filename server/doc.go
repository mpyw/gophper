// Package server serves PHP: over HTTP with HTTPHandler, or behind a web
// server over FastCGI with FastCGIServer, like php-fpm.
//
// Both run each request in a fresh php-cgi instance from a gophper.Engine.
package server
