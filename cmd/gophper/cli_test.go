package main_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mpyw/gophper/internal/fcgi"
)

// These tests run the built gophper as a subprocess, as a user would. What
// they check is the command line: flags, subcommands, exit codes, and what
// each subcommand wires together. The packages it drives have tests of
// their own.
//
// go test counts only its own process, so the subprocess's statements would
// read as untested. With GOPHPER_TEST_COVERDIR set, TestMain builds the
// binary with -cover, and each run writes its counters there. coverage.sh
// merges them with go test's profile. Without it, the binary is built and
// run plainly.

var (
	// cliBinary is the gophper built for these tests.
	cliBinary string
	// cliCacheDir is shared by every run, so that the PHP binaries are
	// compiled once.
	cliCacheDir string
	// cliCoverDir is the absolute GOCOVERDIR, or "" when coverage is off.
	cliCoverDir string
)

func TestMain(m *testing.M) {
	os.Exit(cliMain(m))
}

func cliMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gophper-cli-test-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cliBinary = filepath.Join(dir, "gophper")
	if runtime.GOOS == "windows" {
		cliBinary += ".exe"
	}
	cliCacheDir = filepath.Join(dir, "cache")

	args := []string{"build", "-o", cliBinary}
	if d := os.Getenv("GOPHPER_TEST_COVERDIR"); d != "" {
		if cliCoverDir, err = filepath.Abs(d); err == nil {
			err = os.MkdirAll(cliCoverDir, 0o755)
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "GOPHPER_TEST_COVERDIR:", err)
			return 1
		}
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/mpyw/gophper/...")
	}
	build := exec.Command("go", append(args, ".")...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "building gophper:", err)
		return 1
	}
	return m.Run()
}

// cliCommand prepares a run of gophper with the shared cache. extra comes
// first, as global options do.
func cliCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(cliBinary, append([]string{"--cache-dir", cliCacheDir}, args...)...)
	cmd.Dir = dir
	// The command's own environment, with PWD for cmd.Dir. HOME stays, as
	// a user's would.
	cmd.Env = cmd.Environ()
	if cliCoverDir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+cliCoverDir)
	}
	return cmd
}

// cliRun runs gophper to the end, and returns its output and exit code.
func cliRun(t *testing.T, dir, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := cliCommand(t, dir, args...)
	var out, errOut bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errOut
	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

// cliServer is a running serve or fcgi.
type cliServer struct {
	addr string
	cmd  *exec.Cmd
	done chan error
	logs *bytes.Buffer
}

var cliListening = regexp.MustCompile(`(?:serving .* on \w+://|FastCGI on )(\S+)`)

// cliStart starts a server and waits until it says where it listens. The
// server is stopped with SIGTERM when the test ends, unless the test did.
func cliStart(t *testing.T, dir string, args ...string) *cliServer {
	t.Helper()
	cmd := cliCommand(t, dir, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	cmd.Stdout = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &cliServer{cmd: cmd, done: make(chan error, 1), logs: logs}
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if m := cliListening.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
		s.done <- cmd.Wait()
	}()
	t.Cleanup(func() { s.stop(t) })
	select {
	case s.addr = <-found:
	case err := <-s.done:
		s.done <- err
		t.Fatalf("gophper %s exited before listening: %v", strings.Join(args, " "), err)
	case <-time.After(2 * time.Minute):
		t.Fatalf("gophper %s did not listen", strings.Join(args, " "))
	}
	return s
}

// stop sends SIGTERM and returns the exit code. It is safe to call twice.
func (s *cliServer) stop(t *testing.T) int {
	t.Helper()
	if s.cmd.ProcessState == nil {
		if runtime.GOOS == "windows" {
			_ = s.cmd.Process.Kill()
		} else {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err := <-s.done:
			s.done <- err
		case <-time.After(time.Minute):
			_ = s.cmd.Process.Kill()
			t.Error("the server did not stop on SIGTERM")
			<-s.done
		}
	}
	return s.cmd.ProcessState.ExitCode()
}

func cliWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliGet(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	res, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestCLIHelp(t *testing.T) {
	out, _, code := cliRun(t, t.TempDir(), "", "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, sub := range []string{"php", "serve", "fcgi", "extension", "licenses"} {
		if !strings.Contains(out, sub) {
			t.Errorf("--help does not list %s:\n%s", sub, out)
		}
	}
}

func TestCLIPHP(t *testing.T) {
	dir := t.TempDir()
	cliWrite(t, filepath.Join(dir, "args.php"), `<?php echo implode(",", array_slice($argv, 1)), "|", stream_get_contents(STDIN), "|", getcwd() === getenv("PWD") ? "cwd" : getcwd();`)
	for _, tt := range []struct {
		name  string
		args  []string
		stdin string
		out   string
		code  int
	}{
		{"code", []string{"-r", `echo 1 + 1;`}, "", "2", 0},
		{"exit code", []string{"-r", `exit(3);`}, "", "", 3},
		{"file, arguments and stdin", []string{"args.php", "a", "b"}, "in", "a,b|in|cwd", 0},
		{"define", []string{"-d", "memory_limit=77M", "-r", `echo ini_get("memory_limit");`}, "", "77M", 0},
		{"fatal error", []string{"-r", `undefined_fn();`}, "", "", 255},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, code := cliRun(t, dir, tt.stdin, append([]string{"php"}, tt.args...)...)
			if code != tt.code || (tt.out != "" && out != tt.out) {
				t.Errorf("exit %d, want %d; stdout %q, want %q; stderr %s", code, tt.code, out, tt.out, errOut)
			}
		})
	}
}

// TestCLIPHPName runs gophper under the name php, as a symlink would.
func TestCLIPHPName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	php := filepath.Join(dir, "php")
	if err := os.Symlink(cliBinary, php); err != nil {
		t.Fatal(err)
	}
	// Under the name php, every argument goes to PHP, so the cache comes
	// from the environment.
	cmd := exec.Command(php, "-r", `echo PHP_OS;`)
	cmd.Env = append(cmd.Environ(), "GOPHPER_CACHE_DIR="+cliCacheDir)
	if cliCoverDir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+cliCoverDir)
	}
	out, err := cmd.Output()
	if err != nil || string(out) != "WASI" {
		t.Errorf("%q, %v", out, err)
	}
}

// TestCLIPHPBinary starts PHP again through PHP_BINARY, as Composer does.
func TestCLIPHPBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PHP_BINARY is a shell script")
	}
	out, errOut, code := cliRun(t, t.TempDir(), "", "php", "-r",
		`echo shell_exec(escapeshellarg(PHP_BINARY) . " -r 'echo 6 * 7;'");`)
	if code != 0 || out != "42" {
		t.Errorf("exit %d: %q %s", code, out, errOut)
	}
}

func TestCLIExtensions(t *testing.T) {
	dir := t.TempDir()
	out, _, code := cliRun(t, dir, "", "extension", "list")
	if code != 0 || !strings.Contains(out, "dl_test") || !strings.Contains(out, "intl") {
		t.Fatalf("exit %d: %s", code, out)
	}

	_, errOut, code := cliRun(t, dir, "", "extension", "install", "dl_test")
	if code != 1 || !strings.Contains(errOut, "--extension-dir") {
		t.Errorf("without --extension-dir: exit %d, %s", code, errOut)
	}
	ext := filepath.Join(dir, "ext")
	_, errOut, code = cliRun(t, dir, "", "--extension-dir", ext, "extension", "install")
	if code != 1 || !strings.Contains(errOut, "name an extension") {
		t.Errorf("without a name: exit %d, %s", code, errOut)
	}
	_, errOut, code = cliRun(t, dir, "", "--extension-dir", ext, "extension", "install", "nonexistent")
	if code != 1 {
		t.Errorf("an unknown extension: exit %d, %s", code, errOut)
	}

	out, errOut, code = cliRun(t, dir, "", "--extension-dir", ext, "extension", "install", "dl_test", "intl")
	if code != 0 {
		t.Fatalf("install: exit %d, %s", code, errOut)
	}
	for _, f := range []string{"dl_test.so", "intl.so"} {
		if _, err := os.Stat(filepath.Join(ext, f)); err != nil {
			t.Errorf("%s: %v (%s)", f, err, out)
		}
	}
	out, errOut, code = cliRun(t, dir, "", "--extension-dir", ext, "php", "-d", "extension=dl_test", "-r", `echo dl_test_test2("ext");`)
	if code != 0 || out != `Hello ext` {
		t.Errorf("loading it: exit %d, %q %s", code, out, errOut)
	}
}

func TestCLILicenses(t *testing.T) {
	out, _, code := cliRun(t, t.TempDir(), "", "licenses")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"MIT License", "---- php ----", "---- Go (the standard library and runtime): LICENSE ----", "github.com/tetratelabs/wazero"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q", want)
		}
	}
}

func TestCLIServe(t *testing.T) {
	dir := t.TempDir()
	cliWrite(t, filepath.Join(dir, "public", "index.php"), `<?php
echo json_encode([
	"env" => getenv("APP_ENV"),
	"memory_limit" => ini_get("memory_limit"),
	"max_execution_time" => ini_get("max_execution_time"),
	"ro" => is_writable("/ro") ? "writable" : "read-only",
	"tmp" => is_writable(sys_get_temp_dir()),
	"opcache" => (bool) ini_get("opcache.enable"),
]);`)
	cliWrite(t, filepath.Join(dir, "public", "static.txt"), "static")
	cliWrite(t, filepath.Join(dir, "php.ini"), "memory_limit=77M\nmax_execution_time=7\n[PATH=/nowhere]\n")
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "access.log")

	s := cliStart(t, dir, "serve", "--listen", "127.0.0.1:0", "--root", "public",
		"--mount", ".", "--mount", ro+":ro", "--env", "APP_ENV=test",
		"-c", "php.ini", "-d", "memory_limit=99M", "--temp-dir", t.TempDir(),
		"--concurrency", "2", "--max-wait-time", "5s", "--no-opcache", "--access-log", log)
	code, body := cliGet(t, http.DefaultClient, "http://"+s.addr+"/")
	want := `{"env":"test","memory_limit":"99M","max_execution_time":"7","ro":"read-only","tmp":true,"opcache":false}`
	if code != 200 || body != want {
		t.Errorf("%d %s\nwant %s", code, body, want)
	}
	if code, body := cliGet(t, http.DefaultClient, "http://"+s.addr+"/static.txt"); code != 200 || body != "static" {
		t.Errorf("static: %d %q", code, body)
	}
	if code := s.stop(t); code != 0 {
		t.Errorf("exit %d after SIGTERM\n%s", code, s.logs)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "GET /static.txt") {
		t.Errorf("access log: %q", b)
	}
}

// TestCLIServeModes covers the options that change how PHP runs.
func TestCLIServeModes(t *testing.T) {
	dir := t.TempDir()
	cliWrite(t, filepath.Join(dir, "index.php"), `<?php echo json_encode([(bool) ini_get("opcache.enable"), function_exists("proc_open") && @proc_open("true", [], $p) !== false]);`)
	cliWrite(t, filepath.Join(dir, "router.php"), `<?php if ($_SERVER["REQUEST_URI"] === "/routed") { echo "router"; return; } return false;`)
	for _, tt := range []struct {
		name string
		args []string
		path string
		want string
	}{
		{"workers and opcache", []string{"--opcache-dir", t.TempDir(), "--max-requests", "2"}, "/", `[true,true]`},
		{"fresh instances", []string{"--no-workers"}, "/", `[true,true]`},
		{"no processes", []string{"--no-processes"}, "/", `[true,false]`},
		{"router", []string{"--router", "router.php"}, "/routed", `router`},
		{"front controller off", []string{"--front-controller", "off"}, "/missing", "404 page not found\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := cliStart(t, dir, append([]string{"serve", "-l", "127.0.0.1:0"}, tt.args...)...)
			if _, body := cliGet(t, http.DefaultClient, "http://"+s.addr+tt.path); body != tt.want {
				t.Errorf("%q, want %q", body, tt.want)
			}
		})
	}
}

func TestCLIServeTLS(t *testing.T) {
	dir := t.TempDir()
	cliWrite(t, filepath.Join(dir, "index.php"), `<?php echo $_SERVER["HTTPS"] ?? "off";`)
	cert, key := cliCertificate(t, dir)
	s := cliStart(t, dir, "serve", "-l", "127.0.0.1:0", "--tls-cert", cert, "--tls-key", key)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if code, body := cliGet(t, client, "https://"+s.addr+"/"); code != 200 || body != "on" {
		t.Errorf("%d %q", code, body)
	}
}

// cliCertificate writes a self-signed certificate for 127.0.0.1.
func cliCertificate(t *testing.T, dir string) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cliWrite(t, cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	cliWrite(t, key, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return cert, key
}

func TestCLIServeErrors(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"cert without key", []string{"--tls-cert", "x.pem"}, "go together"},
		{"domain and cert", []string{"--domain", "example.com", "--tls-cert", "x", "--tls-key", "y"}, "drop --tls-cert"},
		{"bad body size", []string{"--max-body", "lots"}, "--max-body"},
		{"bad env", []string{"--env", "NOEQUALS"}, "key=value"},
		{"missing php.ini", []string{"-c", "missing.ini"}, "missing.ini"},
		{"root outside mounts", []string{"--root", "/", "--mount", dir}, "mount"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, errOut, code := cliRun(t, dir, "", append([]string{"serve", "-l", "127.0.0.1:0"}, tt.args...)...)
			if code != 1 || !strings.Contains(errOut, tt.want) {
				t.Errorf("exit %d, want 1 with %q: %s", code, tt.want, errOut)
			}
		})
	}
}

func TestCLIFastCGI(t *testing.T) {
	dir := t.TempDir()
	cliWrite(t, filepath.Join(dir, "index.php"), `<?php echo "fcgi ", $_GET["q"];`)
	listens := []string{"127.0.0.1:0"}
	if runtime.GOOS != "windows" {
		listens = append(listens, "unix:"+filepath.Join(t.TempDir(), "php.sock"))
	}
	for _, listen := range listens {
		t.Run(listen, func(t *testing.T) {
			s := cliStart(t, dir, "fcgi", "--listen", listen, "--ping-path", "/ping")
			network, addr := "tcp", s.addr
			if path, ok := strings.CutPrefix(listen, "unix:"); ok {
				network, addr = "unix", path
				if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o660 {
					t.Errorf("socket mode: %v %v", fi, err)
				}
			}
			for _, tt := range []struct{ uri, want string }{
				{"/index.php?q=1", "fcgi 1"},
				{"/ping", "pong\n"},
			} {
				conn, err := net.Dial(network, addr)
				if err != nil {
					t.Fatal(err)
				}
				script := filepath.Join(dir, "index.php")
				params := map[string]string{
					"REQUEST_METHOD": "GET", "SCRIPT_FILENAME": script, "SCRIPT_NAME": "/index.php",
					"REQUEST_URI": tt.uri, "QUERY_STRING": strings.TrimPrefix(tt.uri[strings.Index(tt.uri+"?", "?"):], "?"),
					"SERVER_PROTOCOL": "HTTP/1.1",
				}
				var out, errOut bytes.Buffer
				_, err = fcgi.Do(context.Background(), conn, params, nil, &out, &errOut)
				_ = conn.Close()
				if err != nil || !strings.HasSuffix(out.String(), tt.want) {
					t.Errorf("%s: %v %q %s", tt.uri, err, out.String(), errOut.String())
				}
			}
		})
	}
}

func TestCLIFastCGIErrors(t *testing.T) {
	_, errOut, code := cliRun(t, t.TempDir(), "", "fcgi", "--listen-mode", "9z")
	if code != 1 || !strings.Contains(errOut, "--listen-mode") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}
