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
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
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
