package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mpyw/gophper"
)

// HTTPHandler serves a PHP application over HTTP in one process, with no
// web server in front. It routes like Caddy's php_server:
//
//   - An existing .php file runs, and the rest of the path becomes PATH_INFO
//     (/index.php/users/1).
//   - Any other existing file is sent as is.
//   - A directory runs its index.php.
//   - Anything else runs the root's index.php, the front controller.
//
// Each request runs in a fresh php-cgi instance.
type HTTPHandler struct {
	pool     *pool
	root     string
	errorLog io.Writer
}

// HTTPConfig configures an HTTPHandler.
type HTTPConfig struct {
	// Root is the document root. It is the only host directory PHP may
	// access, mounted at the same path.
	Root string
	// TempDir is mounted at /tmp inside PHP. Empty means os.TempDir().
	TempDir string
	// Workers limits concurrent PHP instances. Zero means runtime.NumCPU().
	Workers int
	// INI holds php.ini lines such as "max_execution_time=30".
	INI []string
	// ErrorLog receives what PHP writes to stderr. Nil means os.Stderr.
	ErrorLog io.Writer
}

// NewHTTPHandler prepares a handler. Close it to remove its php.ini.
func NewHTTPHandler(engine *gophper.Engine, cfg HTTPConfig) (*HTTPHandler, error) {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	pool, err := newPool(engine, root, cfg.TempDir, cfg.Workers, cfg.INI)
	if err != nil {
		return nil, err
	}
	h := &HTTPHandler{pool: pool, root: root, errorLog: cfg.ErrorLog}
	if h.errorLog == nil {
		h.errorLog = os.Stderr
	}
	return h, nil
}

// Close releases the handler. In-flight requests must have finished.
func (h *HTTPHandler) Close() error {
	return h.pool.close()
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := h.route(r.URL.Path)
	switch {
	case route.redirect != "":
		target := route.redirect
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	case route.static != "":
		http.ServeFile(w, r, route.static)
	case route.script != "":
		h.servePHP(w, r, route)
	default:
		http.NotFound(w, r)
	}
}

// httpRoute is where a request path leads. At most one field is set.
type httpRoute struct {
	// script is the URL path of the PHP script, and pathInfo what follows it.
	script, pathInfo string
	// static is the host path of a file to send.
	static string
	// redirect adds the trailing slash to a directory.
	redirect string
}

func (h *HTTPHandler) route(urlPath string) httpRoute {
	clean := path.Clean("/" + urlPath)

	// /a/b.php/c/d: the first .php segment that is a file is the script.
	for i := 0; ; {
		j := strings.Index(clean[i:], ".php")
		if j < 0 {
			break
		}
		end := i + j + len(".php")
		if end == len(clean) || clean[end] == '/' {
			if h.isFile(clean[:end]) {
				return httpRoute{script: clean[:end], pathInfo: clean[end:]}
			}
		}
		i = end
	}

	full := h.hostPath(clean)
	if fi, err := os.Stat(full); err == nil {
		if !fi.IsDir() {
			return httpRoute{static: full}
		}
		index := path.Join(clean, "index.php")
		if h.isFile(index) {
			if !strings.HasSuffix(urlPath, "/") {
				return httpRoute{redirect: strings.TrimSuffix(clean, "/") + "/"}
			}
			return httpRoute{script: index}
		}
	}
	if h.isFile("/index.php") {
		return httpRoute{script: "/index.php"}
	}
	return httpRoute{}
}

func (h *HTTPHandler) hostPath(urlPath string) string {
	return filepath.Join(h.root, filepath.FromSlash(urlPath))
}

func (h *HTTPHandler) isFile(urlPath string) bool {
	fi, err := os.Stat(h.hostPath(urlPath))
	return err == nil && fi.Mode().IsRegular()
}

func (h *HTTPHandler) servePHP(w http.ResponseWriter, r *http.Request, route httpRoute) {
	body := io.Reader(r.Body)
	contentLength := r.ContentLength
	if contentLength < 0 {
		// php-cgi reads exactly CONTENT_LENGTH bytes, so a chunked body is
		// buffered to learn its length.
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body, contentLength = bytes.NewReader(b), int64(len(b))
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := h.pool.run(r.Context(), h.env(r, route, contentLength), body, pw, h.errorLog)
		pw.CloseWithError(err)
		done <- err
	}()
	// PHP must never block on a full pipe, whatever happens below.
	defer func() {
		io.Copy(io.Discard, pr)
		if err := <-done; err != nil && r.Context().Err() == nil {
			fmt.Fprintf(h.errorLog, "gophper: %s: %v\n", route.script, err)
		}
	}()

	br := bufio.NewReader(pr)
	hdr, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	status := http.StatusOK
	if s := hdr.Get("Status"); s != "" {
		if code, err := strconv.Atoi(strings.Fields(s)[0]); err == nil {
			status = code
		}
		hdr.Del("Status")
	} else if hdr.Get("Location") != "" {
		status = http.StatusFound
	}
	for k, vs := range hdr {
		w.Header()[k] = vs
	}
	w.WriteHeader(status)

	// Copy as PHP writes, so flush() in PHP reaches the client.
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if br.Buffered() == 0 {
				rc.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// env builds the CGI environment, with the variables Caddy's FastCGI
// transport sends.
func (h *HTTPHandler) env(r *http.Request, route httpRoute, contentLength int64) []string {
	remoteHost, remotePort, _ := net.SplitHostPort(r.RemoteAddr)
	serverName, serverPort, err := net.SplitHostPort(r.Host)
	if err != nil {
		serverName = r.Host
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if serverPort == "" {
		serverPort = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	serverAddr := ""
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		serverAddr, _, _ = net.SplitHostPort(la.String())
	}

	vars := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_SOFTWARE":   "gophper",
		"SERVER_PROTOCOL":   r.Proto,
		"SERVER_NAME":       serverName,
		"SERVER_PORT":       serverPort,
		"SERVER_ADDR":       serverAddr,
		"REMOTE_ADDR":       remoteHost,
		"REMOTE_PORT":       remotePort,
		"REQUEST_METHOD":    r.Method,
		"REQUEST_SCHEME":    scheme,
		"REQUEST_URI":       r.RequestURI,
		"QUERY_STRING":      r.URL.RawQuery,
		"DOCUMENT_ROOT":     h.root,
		"DOCUMENT_URI":      route.script + route.pathInfo,
		"SCRIPT_NAME":       route.script,
		"SCRIPT_FILENAME":   h.hostPath(route.script),
		"CONTENT_LENGTH":    strconv.FormatInt(contentLength, 10),
		"CONTENT_TYPE":      r.Header.Get("Content-Type"),
		"HTTP_HOST":         r.Host,
	}
	// Set only when there is one, as Apache and PHP's built-in server do.
	if route.pathInfo != "" {
		vars["PATH_INFO"] = route.pathInfo
		vars["PATH_TRANSLATED"] = h.hostPath(route.pathInfo)
	}
	if r.TLS != nil {
		vars["HTTPS"] = "on"
	}
	for k, vs := range r.Header {
		switch k {
		case "Content-Type", "Content-Length":
			continue
		case "Proxy":
			// httpoxy: HTTP_PROXY would be read as a proxy setting.
			continue
		}
		sep := ", "
		if k == "Cookie" {
			sep = "; "
		}
		vars["HTTP_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))] = strings.Join(vs, sep)
	}

	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}
