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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mpyw/gophper"
)

// runPHP runs code with the host file system mounted and host access, as the CLI does.
//
//declscope:shared // net_test.go and database_test.go
func runPHP(t *testing.T, code string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	exit, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args:   []string{"-r", code},
		Stdout: &out,
		Stderr: &out,
		// As the CLI: the whole host is mounted.
		FS:        gophper.HostFS(),
		HostPath:  gophper.HostPaths,
		Processes: true,
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
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = fmt.Fprintf(c, "echo: %s", line)
				}
			}()
		}
	}()
	return l
}

func TestSocketTCP(t *testing.T) {
	l := echoServer(t, "tcp", "127.0.0.1:0")
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address: %T", l.Addr())
	}
	out, code := runPHP(t, fmt.Sprintf(`
		$fp = fsockopen("127.0.0.1", %d, $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "hello\n");
		echo fgets($fp);
		fwrite($fp, str_repeat("x", 300000) . "\n");
		echo strlen(fgets($fp, 400000)), "\n";
		$name = stream_socket_get_name($fp, true);
		echo $name === "127.0.0.1:%[1]d" ? "peer ok" : "peer $name", "\n";
		fclose($fp);
	`, addr.Port))
	if code != 0 || out != "echo: hello\n300007\npeer ok\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketHTTPWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		_, _ = fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path)
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
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address: %T", l.Addr())
	}
	out, code := runPHP(t, fmt.Sprintf(`
		echo gethostbyname("localhost"), "\n";
		$fp = stream_socket_client("tcp://localhost:%d", $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "by name\n");
		echo fgets($fp);
		echo gethostbyname("no-such-host.invalid"), "\n";
	`, addr.Port))
	if code != 0 || out != "127.0.0.1\necho: by name\nno-such-host.invalid\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address: %T", l.Addr())
	}
	port := addr.Port
	_ = l.Close()
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
	dir, err := os.MkdirTemp(netShortTempBase(), "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "echo.sock")
	echoServer(t, "unix", path)
	out, code := runPHP(t, fmt.Sprintf(`
		$fp = stream_socket_client("unix://%s", $errno, $errstr, 5) or die("$errno $errstr");
		fwrite($fp, "over unix\n");
		echo fgets($fp);
	`, gophper.HostToGuest(path)))
	if code != 0 || out != "echo: over unix\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// TestSocketUnixSandbox connects to a host Unix socket with no HostPath.
// The socket's path maps to no host file, so a sandbox cannot reach it, as
// it cannot reach Docker's.
func TestSocketUnixSandbox(t *testing.T) {
	dir, err := os.MkdirTemp(netShortTempBase(), "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "echo.sock")
	echoServer(t, "unix", path)
	var out bytes.Buffer
	_, err = newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args: []string{"-r", fmt.Sprintf(`var_dump(@stream_socket_client("unix://%s", $errno, $errstr, 5), $errno);`,
			gophper.HostToGuest(path))},
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "bool(false)\nint(44)\n"; out.String() != want {
		t.Errorf("got %q, want %q (ENOENT)", out.String(), want)
	}
}

// TestSocketUnixRelative binds and connects to a relative path, against
// the working directory PHP changed to.
func TestSocketUnixRelative(t *testing.T) {
	dir, err := os.MkdirTemp(netShortTempBase(), "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out, code := runPHP(t, fmt.Sprintf(`
		chdir(%q);
		$s = stream_socket_server("unix://rel.sock", $errno, $errstr) or die("$errno $errstr");
		$c = stream_socket_client("unix://rel.sock", $errno, $errstr, 5) or die("$errno $errstr");
		echo "connected\n";
	`, gophper.HostToGuest(dir)))
	if code != 0 || out != "connected\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
	// Checked from the host: on Windows, a socket file is a reparse point
	// that WASI does not report.
	if _, err := os.Lstat(filepath.Join(dir, "rel.sock")); err != nil {
		t.Errorf("not bound in the directory PHP changed to: %v", err)
	}
}

// TestSocketUnixMappedName binds a Unix socket at a path that HostPath puts
// elsewhere. PHP reads back the path it gave, not the host's.
func TestSocketUnixMappedName(t *testing.T) {
	dir, err := os.MkdirTemp(netShortTempBase(), "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var out bytes.Buffer
	_, err = newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args: []string{"-r", `
			$s = stream_socket_server("unix:///run/php.sock", $errno, $errstr) or die("$errno $errstr");
			$c = stream_socket_client("unix:///run/php.sock", $errno, $errstr, 5) or die("$errno $errstr");
			echo stream_socket_get_name($s, false), " ", stream_socket_get_name($c, true), "\n";
		`},
		HostPath: func(path string) (string, bool, bool) {
			if path != "/run/php.sock" {
				return "", false, false
			}
			return filepath.Join(dir, "host.sock"), true, true
		},
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/run/php.sock /run/php.sock\n"; out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestSocketUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := pc.WriteTo(append([]byte("echo: "), buf[:n]...), from); err != nil {
				t.Error(err)
				return
			}
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
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address: %T", l.Addr())
	}
	go func() {
		c, err := l.Accept()
		if err == nil {
			time.Sleep(5 * time.Second)
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = newTestEngine(t).RunCLI(ctx, gophper.Options{
		Args: []string{"-r", fmt.Sprintf(`$fp = fsockopen("127.0.0.1", %d); fgets($fp);`, addr.Port)},
		FS:   gophper.HostFS(),
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

// TestSocketDNSRecords queries the host's name servers. It needs the
// internet, and skips without it.
func TestSocketDNSRecords(t *testing.T) {
	if _, err := net.LookupMX("gmail.com"); err != nil {
		t.Skipf("no DNS: %v", err)
	}
	out, code := runPHP(t, `
		var_export([
			checkdnsrr("gmail.com", "MX"),
			getmxrr("gmail.com", $mx) && count($mx) > 0,
			dns_get_record("gmail.com", DNS_MX)[0]["type"] ?? null,
			checkdnsrr("no-such-host.invalid", "A"),
		]);
	`)
	if want := "array (\n  0 => true,\n  1 => true,\n  2 => 'MX',\n  3 => false,\n)"; code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketPairInPHP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the child is sh, which Windows lacks")
	}
	out, code := runPHP(t, `
		[$a, $b] = stream_socket_pair(STREAM_PF_UNIX, STREAM_SOCK_STREAM, STREAM_IPPROTO_IP);
		fwrite($a, "to b\n");
		echo fgets($b);
		fwrite($b, "to a\n");
		echo fgets($a);
		fclose($b);
		var_dump(fread($a, 10), feof($a));

		// A child process can be given an end as is.
		[$parent, $child] = stream_socket_pair(STREAM_PF_UNIX, STREAM_SOCK_STREAM, STREAM_IPPROTO_IP);
		$p = proc_open(["sh", "-c", "read line; echo \"child got: \$line\""], [0 => $child, 1 => $child], $pipes);
		fclose($child);
		fwrite($parent, "hello\n");
		echo fgets($parent);
		echo "exit ", proc_close($p), "\n";
	`)
	want := "to b\nto a\nstring(0) \"\"\nbool(true)\nchild got: hello\nexit 0\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketReverseDNS(t *testing.T) {
	want := "127.0.0.1" // PHP's answer when the host has no name for it
	if names, err := net.DefaultResolver.LookupAddr(context.Background(), "127.0.0.1"); err == nil && len(names) > 0 {
		want = strings.TrimSuffix(names[0], ".")
	}
	out, code := runPHP(t, `echo gethostbyaddr("127.0.0.1"), "\n";`)
	if code != 0 || out != want+"\n" {
		t.Errorf("exit %d\n%s\nwant %s", code, out, want)
	}
}

// A UDP server in PHP: recvfrom and sendto, and the errors PHP warns about.
func TestSocketUDPServerInPHP(t *testing.T) {
	out, code := runPHP(t, `
		$server = stream_socket_server("udp://127.0.0.1:0", $errno, $errstr, STREAM_SERVER_BIND) or die("$errno $errstr");
		$addr = stream_socket_get_name($server, false);
		$client = stream_socket_client("udp://$addr", $errno, $errstr) or die("$errno $errstr");
		fwrite($client, "hello");
		var_dump(stream_socket_recvfrom($server, 100, STREAM_PEEK));
		var_dump(stream_socket_recvfrom($server, 100, 0, $from));
		echo $from === stream_socket_get_name($client, false) ? "from client" : "from $from", "\n";
		var_dump(stream_socket_sendto($server, "reply", 0, $from));
		var_dump(fread($client, 100));

		// sendto on a connected socket goes to its peer, from its own port.
		var_dump(stream_socket_sendto($client, "again", 0, $addr));
		stream_socket_recvfrom($server, 100, 0, $again);
		echo $again === $from ? "same port" : "port $again", "\n";

		// Bind to an address in use.
		var_dump(@stream_socket_server("udp://$addr", $errno, $errstr, STREAM_SERVER_BIND), $errstr);
		// A write with nowhere to go.
		var_dump(@fwrite($server, "lost"));
		echo error_get_last()["message"], "\n";
	`)
	want := strings.Join([]string{
		`string(5) "hello"`,
		`string(5) "hello"`,
		"from client",
		"int(5)",
		`string(5) "reply"`,
		"int(5)",
		"same port",
		"bool(false)",
		`string(14) "Address in use"`,
		"bool(false)",
		"fwrite(): Send of 4 bytes failed with errno=17 Destination address required",
		"",
	}, "\n")
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSocketUnixDatagramInPHP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SOCK_DGRAM for AF_UNIX")
	}
	dir, err := os.MkdirTemp(netShortTempBase(), "gophper")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out, code := runPHP(t, fmt.Sprintf(`
		$server = stream_socket_server("udg://%[1]s", $errno, $errstr, STREAM_SERVER_BIND) or die("$errno $errstr");
		$client = stream_socket_client("udg://%[1]s", $errno, $errstr) or die("$errno $errstr");
		fwrite($client, "over udg");
		var_dump(fread($server, 100));
	`, gophper.HostToGuest(filepath.Join(dir, "dgram.sock"))))
	if code != 0 || out != "string(8) \"over udg\"\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// STREAM_CLIENT_ASYNC_CONNECT is a non-blocking connect(2): select says
// when it is done, and a failure shows on the first write.
func TestSocketAsyncConnect(t *testing.T) {
	l := echoServer(t, "tcp", "127.0.0.1:0")
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	out, code := runPHP(t, fmt.Sprintf(`
		$flags = STREAM_CLIENT_CONNECT | STREAM_CLIENT_ASYNC_CONNECT;
		$fp = stream_socket_client("tcp://%s", $errno, $errstr, 5, $flags) or die("$errno $errstr");
		$r = null; $w = [$fp]; $e = null;
		echo "writable: ", stream_select($r, $w, $e, 2) > 0 ? "yes" : "no", "\n";
		fwrite($fp, "async\n");
		echo fgets($fp);

		$fp = stream_socket_client("tcp://%s", $errno, $errstr, 5, $flags) or die("$errno $errstr");
		$r = null; $w = [$fp]; $e = null;
		// Only the write set was asked for, so the failure counts once.
		echo "done: ", stream_select($r, $w, $e, 2), "\n";
		var_dump(@fwrite($fp, "x"));
		echo error_get_last()["message"], "\n";
	`, l.Addr(), closedAddr))
	want := "writable: yes\necho: async\ndone: 1\nbool(false)\nfwrite(): Send of 1 bytes failed with errno=14 Connection refused\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A TCP server on an address in use fails at once, as native PHP does.
// PHP 8.6's TLS-capable tcp:// transport lost listen()'s error.
func TestSocketListenInUse(t *testing.T) {
	out, code := runPHP(t, `
		$a = stream_socket_server("tcp://127.0.0.1:0") or die("first");
		$addr = stream_socket_get_name($a, false);
		var_dump(@stream_socket_server("tcp://$addr", $errno, $errstr), $errstr);
	`)
	want := "bool(false)\nstring(14) \"Address in use\"\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// Socket context options reach the host socket, and shutdown closes one way.
func TestSocketOptionsAndShutdown(t *testing.T) {
	out, code := runPHP(t, `
		$opts = ["socket" => ["tcp_nodelay" => true, "so_keepalive" => true, "so_rcvbuf" => 16384, "so_sndbuf" => 16384]];
		$server = stream_socket_server("tcp://127.0.0.1:0", $errno, $errstr, STREAM_SERVER_BIND | STREAM_SERVER_LISTEN, stream_context_create($opts)) or die("$errno $errstr");
		$addr = stream_socket_get_name($server, false);
		$opts["socket"]["bindto"] = "127.0.0.1:0";
		$client = stream_socket_client("tcp://$addr", $errno, $errstr, 5, STREAM_CLIENT_CONNECT, stream_context_create($opts)) or die("$errno $errstr");
		$conn = stream_socket_accept($server, 5) or die("accept failed");
		echo stream_socket_get_name($conn, true) === stream_socket_get_name($client, false) ? "names match" : "names differ", "\n";

		fwrite($client, "last words");
		stream_socket_shutdown($client, STREAM_SHUT_WR);
		var_dump(stream_get_contents($conn), feof($conn));
		fwrite($conn, "still open");
		fclose($conn);
		var_dump(stream_get_contents($client));
		var_dump(@fwrite($client, "x"));
	`)
	want := "names match\nstring(10) \"last words\"\nbool(true)\nstring(10) \"still open\"\nbool(false)\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// netShortTempBase returns where to make a directory for a socket. macOS
// limits socket paths to 104 bytes, so not under t.TempDir(). Windows has
// no /tmp, and its temporary directory is short enough.
func netShortTempBase() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/tmp"
}
