//go:build !unix

//declscope:namespace main

package main

import "os"

// phpSignals are the signals "gophper php" passes to PHP.
var phpSignals = []os.Signal{os.Interrupt}
