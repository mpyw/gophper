package server

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mpyw/gophper"
	"github.com/mpyw/gophper/internal/hostpath"
)

// HTTPHandler serves a PHP application over HTTP in one process, with no
// web server in front.
//
// Without a router, requests are routed like Caddy's php_server:
//
//   - A file ending in a SplitPath suffix runs, and the rest of the path
//     becomes PATH_INFO (/index.php/users/1).
//   - Any other existing file is sent as is, unless NoStatic is set.
//   - A directory runs its first Index file.
//   - Anything else runs FrontController.
//
// With a router, every request runs it first, as with php -S. If it returns
// false, the requested file is sent as is.
//
// Paths with a segment that starts with "." (.env, .git) are not found,
// except under /.well-known.
//
// Each request runs in a fresh php-cgi instance.
type HTTPHandler struct {
	pool *pool
	cfg  HTTPConfig
	// cwd is where the router runs from, as php -S runs it from where it started.
	cwd string
}

// httpBootstrapRouter runs HTTPConfig.Router and reports `return false`,
// which php-cgi cannot report itself. See bootstrap/router.php.
//
//go:embed bootstrap/router.php
var httpBootstrapRouter string

// httpDefaultMaxBodySize is HTTPConfig.MaxBodySize's default.
const httpDefaultMaxBodySize = 64 << 20

// NewHTTPHandler prepares a handler. Close it to remove its php.ini.
func NewHTTPHandler(engine *gophper.Engine, cfg HTTPConfig) (*HTTPHandler, error) {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	cfg.Root = root
	if len(cfg.Mounts) == 0 {
		cfg.Mounts = []Mount{{Dir: root}}
	}
	if len(cfg.Index) == 0 {
		cfg.Index = []string{"index.php"}
	}
	if cfg.FrontController == "" {
		cfg.FrontController = "/index.php"
	}
	cfg.FrontController = "/" + strings.TrimPrefix(cfg.FrontController, "/")
	if len(cfg.SplitPath) == 0 {
		cfg.SplitPath = []string{".php"}
	}
	if cfg.MaxBodySize == 0 {
		cfg.MaxBodySize = httpDefaultMaxBodySize
	}
	if cfg.Router != "" && !filepath.IsAbs(cfg.Router) {
		cfg.Router = filepath.Join(root, cfg.Router)
	}

	p, err := newPool(engine, cfg.PHPConfig, map[string]string{"router.php": httpBootstrapRouter})
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{root, cfg.Router} {
		if dir != "" && !p.mounted(dir) {
			_ = p.close() // the mount error is the one to report
			return nil, fmt.Errorf("%s is outside every mount", dir)
		}
	}
	// The router runs from the directory the server started in, as with
	// php -S, when PHP can reach it. Otherwise, from the document root.
	cwd, err := os.Getwd()
	if err != nil || !p.mounted(cwd) {
		cwd = root
	}
	cfg.Root, cfg.Router, cwd = p.mountSpelling(cfg.Root), p.mountSpelling(cfg.Router), p.mountSpelling(cwd)
	return &HTTPHandler{pool: p, cfg: cfg, cwd: cwd}, nil
}

// Close releases the handler. In-flight requests must have finished.
func (h *HTTPHandler) Close() error {
	return h.pool.close()
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &httpRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		h.pool.logAccess(r.RemoteAddr, r.Method, r.RequestURI, r.Proto, rec.status, rec.size, start)
	}()

	clean := path.Clean("/" + r.URL.Path)
	if httpHidden(clean) || httpUnsafe(clean) {
		http.NotFound(rec, r)
		return
	}
	if h.cfg.Router != "" {
		h.serveRouter(rec, r, clean)
		return
	}
	h.serveRoute(rec, r, h.route(r.URL.Path, clean, !h.cfg.NoFrontController))
}

// serveRoute answers a request as route says.
func (h *HTTPHandler) serveRoute(rec *httpRecorder, r *http.Request, route httpRoute) {
	switch {
	case route.redirect != "":
		target := route.redirect
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(rec, r, target, http.StatusMovedPermanently)
	case route.static != "":
		http.ServeFile(rec, r, route.static)
	case route.script != "":
		h.serveScript(rec, r, route)
	default:
		http.NotFound(rec, r)
	}
}

// httpHidden reports a path with a segment that starts with ".", other
// than .well-known (RFC 8615).
func httpHidden(clean string) bool {
	for seg := range strings.SplitSeq(strings.TrimPrefix(clean, "/"), "/") {
		if strings.HasPrefix(seg, ".") && seg != ".well-known" {
			return true
		}
	}
	return false
}

// isScriptName reports a file name ending in a SplitPath suffix, in any case.
func (h *HTTPHandler) isScriptName(name string) bool {
	lower := httpLowerASCII(name)
	for _, suffix := range h.cfg.SplitPath {
		if strings.HasSuffix(lower, httpLowerASCII(suffix)) {
			return true
		}
	}
	return false
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

// route finds where a path leads. front falls back to the front controller.
func (h *HTTPHandler) route(urlPath, clean string, front bool) httpRoute {
	if script, pathInfo, ok := h.splitScript(clean); ok {
		return httpRoute{script: script, pathInfo: pathInfo}
	}

	full := h.hostPath(clean)
	if fi, err := os.Stat(full); err == nil {
		if !fi.IsDir() {
			if h.cfg.NoStatic || h.isScriptName(full) {
				// A script is never sent as it is, whatever made it miss
				// splitScript: its source may hold secrets.
				return httpRoute{}
			}
			return httpRoute{static: full}
		}
		for _, name := range h.cfg.Index {
			index := path.Join(clean, name)
			if h.isFile(index) {
				if !strings.HasSuffix(urlPath, "/") {
					return httpRoute{redirect: strings.TrimSuffix(clean, "/") + "/"}
				}
				return httpRoute{script: index}
			}
		}
	}
	if front && h.isFile(h.cfg.FrontController) {
		return httpRoute{script: h.cfg.FrontController}
	}
	return httpRoute{}
}

// httpLowerASCII lowers A to Z only. strings.ToLower changes the length of
// some strings, such as invalid UTF-8, and splitScript indexes the original
// with what it finds in the lowered one.
func httpLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// splitScript finds the first SplitPath suffix that ends an existing file:
// /a/b.php/c/d is the script /a/b.php with PATH_INFO /c/d.
func (h *HTTPHandler) splitScript(clean string) (script, pathInfo string, ok bool) {
	for i := 0; i < len(clean); {
		end, found := -1, false
		// Without case: on Windows and macOS, x.PHP is x.php, and must run
		// rather than be sent as it is.
		lower := httpLowerASCII(clean[i:])
		for _, suffix := range h.cfg.SplitPath {
			j := strings.Index(lower, httpLowerASCII(suffix))
			if j < 0 {
				continue
			}
			e := i + j + len(suffix)
			if (end < 0 || e < end) && (e == len(clean) || clean[e] == '/') {
				end, found = e, true
			}
		}
		if !found {
			return "", "", false
		}
		if h.isFile(clean[:end]) {
			return clean[:end], clean[end:], true
		}
		i = end
	}
	return "", "", false
}

// serveRouter runs the router, then serves the path if it returned false.
func (h *HTTPHandler) serveRouter(w *httpRecorder, r *http.Request, clean string) {
	// What php -S would run without a router, which $_SERVER describes.
	target := httpRoute{script: clean}
	switch route := h.route(clean+"/", clean, !h.cfg.NoFrontController); {
	case route.script != "" && route.script == h.cfg.FrontController && !strings.HasPrefix(clean, route.script):
		// php -S passes the whole path to the index it falls back to, but
		// not when the path names it.
		target = httpRoute{script: route.script, pathInfo: clean}
	case route.script != "":
		target = route
	}
	if target.pathInfo == "/" {
		target.pathInfo = ""
	}
	// Read once: the script that runs after a router reads it too.
	body, contentLength, cleanup, ok := h.readBody(w, r)
	if !ok {
		return
	}
	defer cleanup()
	bootstrap := h.pool.bootstrapPath("router.php")
	pass := h.servePHP(w, r, httpRoute{script: target.script, pathInfo: target.pathInfo}, map[string]string{
		"SCRIPT_FILENAME":                bootstrap,
		"GOPHPER_ROUTER_ROUTER":          hostpath.Guest(h.cfg.Router),
		"GOPHPER_ROUTER_CWD":             hostpath.Guest(h.cwd),
		"GOPHPER_ROUTER_SCRIPT_FILENAME": h.guestPath(target.script),
		"GOPHPER_ROUTER_SCRIPT_NAME":     target.script,
		"GOPHPER_ROUTER_PHP_SELF":        target.script + target.pathInfo,
		"GOPHPER_ROUTER_PATH_INFO":       target.pathInfo,
	}, body, contentLength)
	if !pass {
		return
	}
	// As php -S does when the router returns false: a script runs, and
	// only other files are sent. A path that leads nowhere is 404.
	route := h.route(r.URL.Path, clean, false)
	if route.script == "" {
		h.serveRoute(w, r, route)
		return
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h.servePHP(w, r, route, nil, body, contentLength)
}

func (h *HTTPHandler) hostPath(urlPath string) string {
	return filepath.Join(h.cfg.Root, filepath.FromSlash(urlPath))
}

// guestPath is hostPath as PHP sees it.
func (h *HTTPHandler) guestPath(urlPath string) string {
	return hostpath.Guest(h.hostPath(urlPath))
}

func (h *HTTPHandler) isFile(urlPath string) bool {
	fi, err := os.Stat(h.hostPath(urlPath))
	return err == nil && fi.Mode().IsRegular()
}

// readBody reads the request body in full, as httpBufferBody does, or
// answers with an error and reports false.
func (h *HTTPHandler) readBody(w *httpRecorder, r *http.Request) (io.ReadSeeker, int64, func(), bool) {
	body := io.Reader(r.Body)
	if h.cfg.MaxBodySize > 0 {
		if r.ContentLength > h.cfg.MaxBodySize {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return nil, 0, nil, false
		}
		body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodySize)
	}
	if r.ContentLength == 0 {
		return bytes.NewReader(nil), 0, func() {}, true
	}
	buffered, size, cleanup, err := httpBufferBody(body)
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, http.StatusText(status), status)
		return nil, 0, nil, false
	}
	return buffered, size, cleanup, true
}

// serveScript reads the body and runs a script.
func (h *HTTPHandler) serveScript(w *httpRecorder, r *http.Request, route httpRoute) {
	body, contentLength, cleanup, ok := h.readBody(w, r)
	if !ok {
		return
	}
	defer cleanup()
	h.servePHP(w, r, route, nil, body, contentLength)
}

// servePHP runs a script with a body readBody read, and copies its CGI
// response to w. override replaces CGI variables. It returns true, without
// writing a response, when a router asked to pass.
func (h *HTTPHandler) servePHP(w *httpRecorder, r *http.Request, route httpRoute, override map[string]string, body io.Reader, contentLength int64) (pass bool) {
	vars := h.env(r, route, contentLength)
	maps.Copy(vars, override)

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := h.pool.run(r.Context(), vars, body, pw, h.pool.errorLog)
		_ = pw.CloseWithError(err) // always nil
		done <- err
	}()
	var runErr error
	// PHP must never block on a full pipe, whatever happens below.
	defer func() {
		// Its only error is PHP's, which done carries.
		_, _ = io.Copy(io.Discard, pr)
		runErr = <-done
		if runErr != nil && !errors.Is(runErr, errPoolBusy) && r.Context().Err() == nil {
			writePoolLog(h.pool.errorLog, "gophper: %s: %v\n", route.script, runErr)
		}
	}()

	br := bufio.NewReader(pr)
	hdr, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil {
		if errors.Is(err, errPoolBusy) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		} else {
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		}
		return false
	}
	if hdr.Get("X-Gophper-Router") == "pass" {
		return true
	}
	status := http.StatusOK
	if s := hdr.Get("Status"); s != "" {
		// " 0" keeps a value of only Unicode spaces, which textproto keeps,
		// from leaving no field.
		code, err := strconv.Atoi(strings.Fields(s + " 0")[0])
		switch {
		case err != nil:
		case code < 200 || code > 999:
			// net/http panics outside 100-999, and an informational code
			// would let it send a 200 of its own after.
			status = http.StatusBadGateway
		default:
			status = code
		}
		hdr.Del("Status")
	} else if hdr.Get("Location") != "" {
		status = http.StatusFound
	}
	maps.Copy(w.Header(), hdr)
	w.WriteHeader(status)

	// Copy as PHP writes, so flush() in PHP reaches the client.
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return false
			}
			if br.Buffered() == 0 {
				// The client is gone. A writer that cannot flush still writes.
				if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
					return false
				}
			}
		}
		if err != nil {
			return false
		}
	}
}

// env builds the CGI variables, with those Caddy's FastCGI transport sends.
func (h *HTTPHandler) env(r *http.Request, route httpRoute, contentLength int64) map[string]string {
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
		"DOCUMENT_ROOT":     hostpath.Guest(h.cfg.Root),
		"DOCUMENT_URI":      route.script + route.pathInfo,
		"SCRIPT_NAME":       route.script,
		"SCRIPT_FILENAME":   h.guestPath(route.script),
		"CONTENT_LENGTH":    strconv.FormatInt(contentLength, 10),
		"CONTENT_TYPE":      r.Header.Get("Content-Type"),
		"HTTP_HOST":         r.Host,
	}
	// Set only when there is one, as Apache and PHP's built-in server do.
	if route.pathInfo != "" {
		vars["PATH_INFO"] = route.pathInfo
		vars["PATH_TRANSLATED"] = h.guestPath(route.pathInfo)
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
		if strings.Contains(k, "_") {
			// X-Real_IP would pass for X-Real-IP, which a proxy in front
			// may have set. nginx and Apache drop these too.
			continue
		}
		sep := ", "
		if k == "Cookie" {
			sep = "; "
		}
		vars["HTTP_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))] = strings.Join(vs, sep)
	}
	return vars
}

// httpRecorder remembers the status and size for the access log.
type httpRecorder struct {
	http.ResponseWriter
	status int
	size   int64
}

func (rec *httpRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *httpRecorder) Write(b []byte) (int, error) {
	n, err := rec.ResponseWriter.Write(b)
	rec.size += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rec *httpRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// httpBodyMemory is how much of a request body httpBufferBody keeps in
// memory. The rest goes to a temporary file.
const httpBodyMemory = 1 << 20

// httpBufferBody reads a whole request body before PHP runs, as nginx does
// by default (proxy_request_buffering). PHP reads every byte of
// CONTENT_LENGTH before it ends a request, so a client that sent its body
// slowly would hold a PHP instance as long as it liked. It also gives a
// chunked body the length that php-cgi needs.
//
//declscope:shared // fastcgi.go buffers FCGI_STDIN with it
func httpBufferBody(body io.Reader) (io.ReadSeeker, int64, func(), error) {
	var buf bytes.Buffer
	n, err := io.CopyN(&buf, body, httpBodyMemory+1)
	if err == io.EOF {
		return bytes.NewReader(buf.Bytes()), n, func() {}, nil
	}
	if err != nil {
		return nil, 0, nil, err
	}
	f, err := os.CreateTemp("", "gophper-body-*")
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup := func() {
		// A read-only temporary file: nothing is lost if these fail.
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	rest, err := io.Copy(f, io.MultiReader(&buf, body))
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return f, rest, cleanup, nil
}
