package gophper_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	phpext "github.com/mpyw/gophper-wasm/ext"

	"github.com/mpyw/gophper"
)

// newExtensionEngine returns an Engine whose ExtensionDir holds every
// extension that comes with gophper-wasm, such as dl_test, php-src's
// extension for testing dl().
func newExtensionEngine(t *testing.T) *gophper.Engine {
	t.Helper()
	dir := t.TempDir()
	for _, name := range phpext.Names() {
		so, err := phpext.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".so"), so, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, file := range phpext.Files(name) {
			b, err := phpext.OpenFile(name, file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := gophper.DefaultEngineConfig()
	cfg.ExtensionDir = dir
	e, err := gophper.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func runExtension(t *testing.T, e *gophper.Engine, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	// With the network, as redis connects over TCP.
	code, err := e.RunCLI(context.Background(), gophper.Options{Args: args, Stdout: &out, Stderr: &out, Network: true})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func TestExtensionFromINI(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-d", "extension=dl_test", "-r", `
		var_dump(extension_loaded("dl_test"), dl_test_test2("gophper"), ini_get("dl_test.long"));
		echo (new DlTest())->test("method"), "\n";
		try { dl_test_test2([]); } catch (TypeError $e) { echo get_class($e), "\n"; }
	`)
	want := "bool(true)\nstring(13) \"Hello gophper\"\nstring(1) \"0\"\nHello method\nTypeError\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestExtensionFromDL(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-r", `var_dump(extension_loaded("dl_test"), dl("dl_test.so"), dl_test_test2("dl"));`)
	if code != 0 || out != "bool(false)\nbool(true)\nstring(8) \"Hello dl\"\n" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A fatal error while the extension's code is on the stack unwinds through
// it, and shutdown functions still run.
func TestExtensionFatalError(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-d", "extension=dl_test", "-r", `
		register_shutdown_function(fn () => print("shutdown ran\n"));
		class Boom { function __toString(): string { undefined_fn(); return ""; } }
		dl_test_test2(new Boom);
	`)
	if code != 255 || !strings.Contains(out, "Call to undefined function undefined_fn()") || !strings.HasSuffix(out, "shutdown ran\n") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestExtensionMissing(t *testing.T) {
	out, _ := runExtension(t, newExtensionEngine(t), "-d", "extension=nope", "-r", `echo "still runs\n";`)
	if !strings.Contains(out, `Unable to load dynamic library 'nope'`) || !strings.Contains(out, "still runs") {
		t.Errorf("got\n%s", out)
	}
}

func TestExtensionNotSharedLibrary(t *testing.T) {
	dir := t.TempDir()
	// A valid but empty module: no dylink.0 section.
	if err := os.WriteFile(filepath.Join(dir, "plain.so"), []byte("\x00asm\x01\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := gophper.DefaultEngineConfig()
	cfg.ExtensionDir = dir
	e, err := gophper.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	out, _ := runExtension(t, e, "-d", "extension=plain", "-r", `echo "still runs\n";`)
	if !strings.Contains(out, "not a shared library") || !strings.Contains(out, "still runs") {
		t.Errorf("got\n%s", out)
	}
}

// TestExtensionBundled loads every extension that comes with gophper-wasm.
func TestExtensionBundled(t *testing.T) {
	for _, name := range phpext.Names() {
		t.Run(name, func(t *testing.T) {
			out, code := runExtension(t, newExtensionEngine(t), "-d", "extension="+name, "-r", `echo extension_loaded("`+name+`") ? "loaded" : "missing";`)
			if code != 0 || out != "loaded" {
				t.Errorf("exit %d\n%s", code, out)
			}
		})
	}
}

// TestExtensionIntl formats with ICU and its data, which comes beside intl.so.
// The data has English and Japanese only.
func TestExtensionIntl(t *testing.T) {
	out, code := runExtension(t, newExtensionEngine(t), "-d", "extension=intl", "-r", `
		echo (new NumberFormatter("ja_JP", NumberFormatter::CURRENCY))->formatCurrency(1234.5, "JPY"), "\n";
		echo MessageFormatter::formatMessage("en_US", "{0, plural, one{# file} other{# files}}", [3]), "\n";
		echo Normalizer::normalize("e\u{301}") === "\u{e9}" ? "normalized" : "not normalized", "\n";`)
	if want := "￥1,234\n3 files\nnormalized\n"; code != 0 || out != want {
		t.Errorf("exit %d\n%q", code, out)
	}
}

// TestExtensionRedis talks to a fake Redis that speaks enough RESP for the
// client and for session.save_handler=redis.
func TestExtensionRedis(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address: %T", l.Addr())
	}
	// Shared by every connection, as in a real server.
	var data sync.Map
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go redisFake(conn, &data)
		}
	}()
	out, code := runExtension(t, newExtensionEngine(t), "-d", "extension=redis", "-r", fmt.Sprintf(`
		$r = new Redis();
		$r->connect("127.0.0.1", %d);
		var_dump($r->ping(), $r->set("k", "v"), $r->get("k"), $r->get("missing"));`, addr.Port))
	if want := "bool(true)\nbool(true)\nstring(1) \"v\"\nbool(false)\n"; code != 0 || out != want {
		t.Errorf("exit %d\n%s", code, out)
	}

	// session.save_handler=redis: one run writes the session, the next reads it.
	save := fmt.Sprintf("tcp://127.0.0.1:%d", addr.Port)
	session := []string{"-d", "extension=redis", "-d", "session.save_handler=redis", "-d", "session.save_path=" + save,
		"-d", "session.use_cookies=0", "-d", "session.use_strict_mode=0"}
	_, code = runExtension(t, newExtensionEngine(t), append(session, "-r", `session_id("abc"); session_start(); $_SESSION["n"] = 42;`)...)
	out, code2 := runExtension(t, newExtensionEngine(t), append(session, "-r", `session_id("abc"); session_start(); var_dump($_SESSION["n"] ?? null);`)...)
	if code != 0 || code2 != 0 || out != "int(42)\n" {
		t.Errorf("session: exit %d, %d\n%s", code, code2, out)
	}
}

func redisFake(conn net.Conn, data *sync.Map) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
		args := make([]string, n)
		for i := range args {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			arg, _ := r.ReadString('\n')
			args[i] = strings.TrimRight(arg, "\r\n")
		}
		switch strings.ToUpper(args[0]) {
		case "PING":
			_, _ = fmt.Fprint(conn, "+PONG\r\n")
		case "SET":
			data.Store(args[1], args[2])
			_, _ = fmt.Fprint(conn, "+OK\r\n")
		case "SETEX":
			data.Store(args[1], args[3])
			_, _ = fmt.Fprint(conn, "+OK\r\n")
		case "EXPIRE":
			if _, ok := data.Load(args[1]); ok {
				_, _ = fmt.Fprint(conn, ":1\r\n")
			} else {
				_, _ = fmt.Fprint(conn, ":0\r\n")
			}
		case "GET":
			// Only strings are stored, so a non-string is a missing key.
			v, _ := data.Load(args[1])
			if s, ok := v.(string); ok {
				_, _ = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(s), s)
			} else {
				_, _ = fmt.Fprint(conn, "$-1\r\n")
			}
		default:
			_, _ = fmt.Fprint(conn, "-ERR unknown command\r\n")
		}
	}
}
