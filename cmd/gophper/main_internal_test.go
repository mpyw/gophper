//declscope:namespace main

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mpyw/gophper"
)

func TestGuestArgs(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "run.php")
	if err := os.WriteFile(abs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	in := []string{"-r", "echo 1;", abs, "run.php", "plain"}
	want := []string{"-r", "echo 1;", gophper.HostToGuest(abs), "run.php", "plain"}
	if runtime.GOOS == "windows" {
		// A relative path with backslashes takes slashes, if it names a file.
		if err := os.Mkdir("sub", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(`sub\x.php`, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		in = append(in, `sub\x.php`, `no\such`)
		want = append(want, "sub/x.php", `no\such`)
	}
	if got := guestArgs(in); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGuestEnv(t *testing.T) {
	if got := guestEnv([]string{"A=1", "TMPDIR=/x"}); !slices.Equal(got, []string{"A=1", "TMPDIR=/x"}) {
		t.Errorf("TMPDIR set: %q", got)
	}
	want := []string{"A=1", "TMPDIR=" + gophper.HostToGuest(os.TempDir())}
	if got := guestEnv([]string{"A=1"}); !slices.Equal(got, want) {
		t.Errorf("TMPDIR unset: %q, want %q", got, want)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": -1, "10": 10, "64M": 64 << 20, " 2g ": 2 << 30, "1k": 1 << 10} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "lots", "1T"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d", in, got)
		}
	}
}

// TestServeServers builds serve's servers without starting them: plain
// HTTP by default, a certificate's TLS, or Let's Encrypt for domains, with
// :80 for its challenge.
func TestServeServers(t *testing.T) {
	h := http.NotFoundHandler()
	plain, err := serveServers(h, "", nil, "", "")
	if err != nil || len(plain) != 1 || plain[0].Addr != "127.0.0.1:8080" || plain[0].TLSConfig != nil {
		t.Errorf("plain: %v, %v", plain, err)
	}
	acme, err := serveServers(h, "", []string{"gophper.invalid"}, "", "")
	if err != nil || len(acme) != 2 {
		t.Fatalf("acme: %v, %v", acme, err)
	}
	if acme[0].Addr != ":80" || acme[0].Handler == nil || acme[1].Addr != ":443" || acme[1].TLSConfig == nil || acme[1].TLSConfig.GetCertificate == nil {
		t.Errorf("acme: %s %v, %s %v", acme[0].Addr, acme[0].Handler, acme[1].Addr, acme[1].TLSConfig)
	}
	if got, err := serveServers(h, "127.0.0.1:0", []string{"gophper.invalid"}, "", ""); err != nil || got[1].Addr != "127.0.0.1:0" {
		t.Errorf("acme with --listen: %v, %v", got, err)
	}
	cert, key := serveTestCertificate(t)
	tlsSrv, err := serveServers(h, "127.0.0.1:0", nil, cert, key)
	if err != nil || len(tlsSrv) != 1 || tlsSrv[0].TLSConfig == nil || len(tlsSrv[0].TLSConfig.Certificates) != 1 {
		t.Errorf("certificate: %v, %v", tlsSrv, err)
	}
	if _, err := serveServers(h, "", nil, cert+".missing", key); err == nil {
		t.Error("a missing certificate was accepted")
	}
}

// serveFailingListener fails its first Accept, as a listener that broke.
type serveFailingListener struct{ net.Listener }

func (serveFailingListener) Accept() (net.Conn, error) { return nil, errServeBroken }

var errServeBroken = errors.New("broken listener")

// TestServeRunFails ends serve when a server fails, and stops the others.
// A server that cannot listen stops the ones serving already.
func TestServeRunFails(t *testing.T) {
	listen := serveListen
	t.Cleanup(func() { serveListen = listen })
	var first net.Listener
	serveListen = func(network, address string) (net.Listener, error) {
		l, err := listen(network, address)
		if err != nil || first != nil {
			return l, err
		}
		first = l
		return serveFailingListener{l}, nil
	}
	h := http.NotFoundHandler()
	servers := []*http.Server{{Addr: "127.0.0.1:0", Handler: h}, {Addr: "127.0.0.1:0", Handler: h}}
	if err := serveRun(context.Background(), "root", servers); !errors.Is(err, errServeBroken) {
		t.Errorf("a failing server: %v", err)
	}

	var started net.Listener
	serveListen = func(network, address string) (net.Listener, error) {
		if started != nil {
			return nil, errServeBroken
		}
		l, err := listen(network, address)
		started = l
		return l, err
	}
	servers = []*http.Server{{Addr: "127.0.0.1:0", Handler: h}, {Addr: "127.0.0.1:0", Handler: h}}
	if err := serveRun(context.Background(), "root", servers); !errors.Is(err, errServeBroken) {
		t.Errorf("a server that cannot listen: %v", err)
	}
	// The first one was closed, and its listener with it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", started.Addr().String())
		if err != nil {
			break
		}
		_ = c.Close() // only probing
		if time.Now().After(deadline) {
			t.Fatal("the first server still listens")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFCGIListenChmodFails closes the socket when its mode cannot be set.
func TestFCGIListenChmodFails(t *testing.T) {
	// Short: a Unix socket's path is at most about 100 bytes, Windows' too.
	dir, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	chmod := fcgiChmod
	t.Cleanup(func() { fcgiChmod = chmod })
	fcgiChmod = func(string, os.FileMode) error { return os.ErrPermission }
	sock := filepath.Join(dir, "php.sock")
	if l, err := fcgiListen("unix:"+sock, 0o660); !errors.Is(err, os.ErrPermission) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("err = %v", err)
	}
	// Closed: nothing answers there.
	if c, err := net.Dial("unix", sock); err == nil {
		_ = c.Close()
		t.Error("the socket still listens")
	}
}

// serveTestCertificate writes a self-signed certificate and its key.
func serveTestCertificate(t *testing.T) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// mainBrokenWriter fails every write after the first n, as a stdout whose
// reader went away.
type mainBrokenWriter struct{ n int }

func (w *mainBrokenWriter) Write(p []byte) (int, error) {
	if w.n == 0 {
		return 0, errors.New("broken pipe")
	}
	w.n--
	return len(p), nil
}

// TestMainWriteErrors reports a failed write to stdout, at each point it
// can fail, rather than printing on as if all went out.
func TestMainWriteErrors(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		args []string
		n    int
	}{
		{[]string{"licenses"}, 0},
		{[]string{"licenses"}, 1},
		{[]string{"extension", "list"}, 0},
		{[]string{"--extension-dir", dir, "extension", "install", "intl"}, 0},
		{[]string{"--extension-dir", dir, "extension", "install", "intl"}, 1},
	} {
		cmd := newRootCommand()
		cmd.Writer, cmd.ErrWriter = &mainBrokenWriter{n: tt.n}, io.Discard
		if err := cmd.Run(context.Background(), append([]string{"gophper"}, tt.args...)); err == nil || !strings.Contains(err.Error(), "broken pipe") {
			t.Errorf("%v after %d writes: %v", tt.args, tt.n, err)
		}
	}
}

// mainUnresolvable returns a relative path that filepath.Abs fails on, and
// the error it fails with. On Linux, getcwd(2) fails once the working
// directory is removed. Windows refuses a NUL, though it keeps the working
// directory from being removed. macOS resolves both, so it has no such path.
func mainUnresolvable(t *testing.T) (string, error) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		dir := filepath.Join(t.TempDir(), "gone")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		return "rel", syscall.ENOENT
	case "windows":
		return "rel\x00", syscall.EINVAL
	}
	t.Skip("only Linux and Windows have a relative path that filepath.Abs cannot resolve")
	return "", nil
}

// TestMainUnresolvablePaths reports each option whose relative path cannot
// be made absolute, rather than going on with a path that means nothing.
func TestMainUnresolvablePaths(t *testing.T) {
	// PHP_BINARY's script goes here, not in the user's own directories.
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	mount := t.TempDir()
	rel, want := mainUnresolvable(t)
	for _, args := range [][]string{
		{"--cache-dir", rel, "serve", "--mount", mount},
		{"--no-cache", "--extension-dir", rel, "serve", "--mount", mount},
		{"--no-cache", "serve", "--mount", rel},
		{"--no-cache", "serve", "--mount", mount, "--root", rel},
		{"--no-cache", "serve", "--mount", mount, "--temp-dir", rel},
	} {
		cmd := newRootCommand()
		cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
		if err := cmd.Run(context.Background(), append([]string{"gophper"}, args...)); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", args, err, want)
		}
	}
	// php starts in the working directory, which Linux cannot name.
	if runtime.GOOS == "linux" {
		cmd := newRootCommand()
		cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
		if err := cmd.Run(context.Background(), []string{"gophper", "--no-cache", "php", "-r", ""}); !errors.Is(err, want) {
			t.Errorf("php: %v, want %v", err, want)
		}
	}
}

// TestExtensionInstallBroken: an extension, or a file beside it, that
// cannot be read, as from a broken copy of gophper-wasm.
func TestExtensionInstallBroken(t *testing.T) {
	open, openFile := extensionOpen, extensionOpenFile
	t.Cleanup(func() { extensionOpen, extensionOpenFile = open, openFile })
	broken := errors.New("gzip: invalid header")
	for name, set := range map[string]func(){
		"extension": func() { extensionOpen = func(string) ([]byte, error) { return nil, broken } },
		"its file":  func() { extensionOpenFile = func(string, string) ([]byte, error) { return nil, broken } },
	} {
		extensionOpen, extensionOpenFile = open, openFile
		set()
		cmd := newRootCommand()
		cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
		// intl has its ICU data beside it.
		if err := cmd.Run(context.Background(), []string{"gophper", "--extension-dir", t.TempDir(), "extension", "install", "intl"}); !errors.Is(err, broken) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// mainBrokenFS lists one file, which it cannot open; with list unset, it
// cannot list either.
type mainBrokenFS struct{ list bool }

func (f mainBrokenFS) Open(name string) (fs.File, error) {
	if name == "." && f.list {
		return fstest.MapFS{"php.txt": {}}.Open(".")
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

// TestLicensesUnreadable reports licenses that cannot be listed or read,
// rather than printing fewer of them.
func TestLicensesUnreadable(t *testing.T) {
	defer func(old fs.FS) { licensesFiles = old }(licensesFiles)
	for _, list := range []bool{false, true} {
		licensesFiles = mainBrokenFS{list: list}
		cmd := newRootCommand()
		cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
		if err := cmd.Run(context.Background(), []string{"gophper", "licenses"}); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("listing works %v: %v", list, err)
		}
	}
}
