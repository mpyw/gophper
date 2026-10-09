package gophper_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
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
