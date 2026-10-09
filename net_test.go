package gophper_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

// runPHP runs code with the host file system mounted, as the CLI does.
func runPHP(t *testing.T, code string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	exit, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args:   []string{"-r", code},
		Stdout: &out,
		Stderr: &out,
		FS:     wazero.NewFSConfig().WithDirMount("/", "/"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), exit
}

// echoServer answers each line with "echo: <line>".
func echoServer(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					fmt.Fprintf(c, "echo: %s", line)
				}
			}()
		}
	}()
	return l
}

func TestSocketTCP(t *testing.T) {
	l := echoServer(t, "tcp", "127.0.0.1:0")
	out, code := runPHP(t, fmt.Sprintf(`
		$fp = fsockopen("127.0.0.1", %d, $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "hello\n");
		echo fgets($fp);
		fwrite($fp, str_repeat("x", 300000) . "\n");
		echo strlen(fgets($fp, 400000)), "\n";
		$name = stream_socket_get_name($fp, true);
		echo $name === "127.0.0.1:%[1]d" ? "peer ok" : "peer $name", "\n";
		fclose($fp);
	`, l.Addr().(*net.TCPAddr).Port))
	if code != 0 || out != "echo: hello\n300007\npeer ok\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketHTTPWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	out, code := runPHP(t, fmt.Sprintf(`
		echo file_get_contents(%q), "\n";
		echo in_array("X-Test: yes", http_get_last_response_headers()) ? "header ok" : "no header", "\n";
	`, srv.URL+"/path"))
	if code != 0 || out != "GET /path\nheader ok\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketDNS(t *testing.T) {
	l := echoServer(t, "tcp", "127.0.0.1:0")
	out, code := runPHP(t, fmt.Sprintf(`
		echo gethostbyname("localhost"), "\n";
		$fp = stream_socket_client("tcp://localhost:%d", $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "by name\n");
		echo fgets($fp);
		echo gethostbyname("no-such-host.invalid"), "\n";
	`, l.Addr().(*net.TCPAddr).Port))
	if code != 0 || out != "127.0.0.1\necho: by name\nno-such-host.invalid\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	out, _ := runPHP(t, fmt.Sprintf(`
		$fp = @fsockopen("127.0.0.1", %d, $errno, $errstr, 5);
		var_dump($fp, $errstr);
	`, port))
	if !strings.Contains(out, "bool(false)") || !strings.Contains(out, "Connection refused") {
		t.Errorf("got\n%s", out)
	}
}

func TestSocketServerInPHP(t *testing.T) {
	out, code := runPHP(t, `
		$server = stream_socket_server("tcp://127.0.0.1:0", $errno, $errstr) or die("$errno $errstr");
		$addr = stream_socket_get_name($server, false);
		$client = stream_socket_client("tcp://$addr", $errno, $errstr, 5) or die("$errno $errstr");
		$conn = stream_socket_accept($server, 5) or die("accept failed");
		fwrite($client, "ping\n");
		echo "server got: ", fgets($conn);
		fwrite($conn, "pong\n");
		echo "client got: ", fgets($client);

		// stream_select reports only the socket with data.
		$other = stream_socket_client("tcp://$addr", $errno, $errstr, 5);
		$otherConn = stream_socket_accept($server, 5);
		fwrite($otherConn, "second\n");
		$r = [$client, $other]; $w = null; $e = null;
		echo "select: ", stream_select($r, $w, $e, 2), " ready, other=", ($r === [1 => $other] ? "yes" : "no"), "\n";

		// Non-blocking read with nothing to read.
		stream_set_blocking($client, false);
		var_dump(fread($client, 10));

		// EOF after the peer closes.
		fclose($conn);
		stream_set_blocking($client, true);
		var_dump(fgets($client), feof($client));
	`)
	want := "server got: ping\nclient got: pong\nselect: 1 ready, other=yes\nstring(0) \"\"\nbool(false)\nbool(true)\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketUnix(t *testing.T) {
	// macOS limits socket paths to 104 bytes, so not under t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "echo.sock")
	echoServer(t, "unix", path)
	out, code := runPHP(t, fmt.Sprintf(`
		$fp = stream_socket_client("unix://%s", $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "over unix\n");
		echo fgets($fp);
	`, path))
	if code != 0 || out != "echo: over unix\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo: "), buf[:n]...), from)
		}
	}()
	out, code := runPHP(t, fmt.Sprintf(`
		$fp = stream_socket_client("udp://%s", $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "datagram");
		echo fread($fp, 100), "\n";
	`, pc.LocalAddr()))
	if code != 0 || out != "echo: datagram\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// Canceling the run stops a script blocked reading a socket. PHP retries
// reads that fail with EINTR, so this must not spin.
func TestSocketCancelWhileReading(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			time.Sleep(5 * time.Second)
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = newTestEngine(t).RunCLI(ctx, gophper.Options{
		Args: []string{"-r", fmt.Sprintf(`$fp = fsockopen("127.0.0.1", %d); fgets($fp);`, l.Addr().(*net.TCPAddr).Port)},
		FS:   wazero.NewFSConfig().WithDirMount("/", "/"),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
}

// fsockopen's timeout goes through a non-blocking connect and poll(2).
func TestSocketConnectTimeout(t *testing.T) {
	start := time.Now()
	// 10.255.255.1 is unroutable on most networks, so the SYN gets no answer.
	out, _ := runPHP(t, `$fp = @fsockopen("10.255.255.1", 80, $errno, $errstr, 0.3); var_dump($fp, $errstr);`)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
	if !strings.Contains(out, "bool(false)") {
		t.Errorf("got\n%s", out)
	}
	t.Logf("errstr: %s", strings.TrimSpace(out[strings.LastIndex(out, "string("):]))
}
