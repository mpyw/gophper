package gophper_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

// TestDatabasePDO connects to real servers. Each DSN comes from an
// environment variable, and its test is skipped without one:
//
//	GOPHPER_TEST_MYSQL_DSN='mysql:host=127.0.0.1;port=3306;dbname=app;user=app;password=secret'
//	GOPHPER_TEST_PGSQL_DSN='pgsql:host=127.0.0.1;port=5432;dbname=app;user=postgres;password=secret'
func TestDatabasePDO(t *testing.T) {
	for _, tt := range []struct {
		driver string
		env    string
	}{
		{"mysql", "GOPHPER_TEST_MYSQL_DSN"},
		{"pgsql", "GOPHPER_TEST_PGSQL_DSN"},
		{"sqlite", ""},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := "sqlite::memory:"
			if tt.env != "" {
				dsn = os.Getenv(tt.env)
				if dsn == "" {
					t.Skipf("%s is not set", tt.env)
				}
			}
			out, exit := runPHP(t, fmt.Sprintf(`
				$pdo = new PDO('%s', null, null, [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
				$pdo->exec('CREATE TEMPORARY TABLE gophper_test (n INT)');
				$st = $pdo->prepare('INSERT INTO gophper_test (n) VALUES (?), (?)');
				$st->execute([20, 22]);
				echo $pdo->query('SELECT SUM(n) FROM gophper_test')->fetchColumn();
			`, strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(dsn)))
			if exit != 0 || strings.TrimSpace(out) != "42" {
				t.Fatalf("exit %d: %s", exit, out)
			}
		})
	}
}

// TestDatabaseSQLiteLocks opens one database from two instances, as two
// workers would. While one holds an exclusive lock, the other must not get
// one, whichever VFS a script picks. WASI has no record locks, so SQLite's
// unix VFS must fail rather than lock nothing.
func TestDatabaseSQLiteLocks(t *testing.T) {
	for _, vfs := range []string{"", "unix"} {
		t.Run("vfs="+vfs, func(t *testing.T) {
			dir := t.TempDir()
			db, held, release := dir+"/t.db", dir+"/held", dir+"/release"
			dsn := "sqlite:" + db
			if vfs != "" {
				dsn = "sqlite:file:" + db + "?vfs=" + vfs
			}
			lock := fmt.Sprintf(`
				$pdo = new PDO('%s', null, null, [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION, PDO::ATTR_TIMEOUT => 0]);
				try { $pdo->exec('BEGIN EXCLUSIVE'); } catch (PDOException $e) { echo 'failed'; exit; }
				echo 'locked';
			`, dsn)
			if out, _ := runPHP(t, fmt.Sprintf(`(new PDO('sqlite:%s'))->exec('CREATE TABLE t (n INT)');`, db)); out != "" {
				t.Fatal(out)
			}
			// runPHP may call t.Fatal, which only the test's goroutine may.
			engine := newTestEngine(t)
			first := make(chan string, 1)
			go func() {
				var out bytes.Buffer
				_, err := engine.RunCLI(context.Background(), gophper.Options{
					Args: []string{"-r", lock + fmt.Sprintf(`
						touch('%s');
						for ($i = 0; $i < 200 && !file_exists('%s'); $i++) usleep(50000);
					`, held, release)},
					Stdout: &out, Stderr: &out,
					FS:       wazero.NewFSConfig().WithDirMount("/", "/"),
					HostPath: func(path string) (string, bool, bool) { return path, true, true },
				})
				if err != nil {
					_, _ = fmt.Fprint(&out, err)
				}
				first <- out.String()
			}()
			for deadline := time.Now().Add(30 * time.Second); ; {
				if _, err := os.Stat(held); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the first instance never locked")
				}
				select {
				case out := <-first:
					// The first could not lock either, so nothing can collide.
					if out != "failed" {
						t.Fatalf("first: %s", out)
					}
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
			second, _ := runPHP(t, lock)
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			<-first
			if second != "failed" {
				t.Errorf("second instance: %s, while the first held an exclusive lock", second)
			}
		})
	}
}
