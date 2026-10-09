// Package gophper runs the PHP interpreter (php-src compiled to WebAssembly)
// on wazero, with no cgo.
//
// An [Engine] compiles the PHP binaries once and runs them many times.
// [FastCGIServer] serves PHP over FastCGI on top of it, like php-fpm.
package gophper
