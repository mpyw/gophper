package server_test

// writerFunc adapts a function to io.Writer.
//
//declscope:shared // fastcgi_test.go and http_test.go collect logs with it
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
