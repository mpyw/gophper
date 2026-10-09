package gophper_test

import (
	"runtime"
	"strings"
	"testing"
)

// TestProcess starts host programs from PHP. It needs a Unix shell.
func TestProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	for _, tt := range []struct {
		name string
		code string
		want string
	}{
		{"exec", `exec('echo a; echo b', $out, $code); echo implode(',', $out), " $code";`, "a,b 0"},
		{"shell_exec", `echo trim(shell_exec('echo hi'));`, "hi"},
		{"system exit code", `ob_start(); system('exit 3', $rc); ob_end_clean(); echo $rc;`, "3"},
		{"proc_open pipes", `
			$p = proc_open(['cat'], [['pipe', 'r'], ['pipe', 'w']], $pipes);
			fwrite($pipes[0], 'through cat'); fclose($pipes[0]);
			echo stream_get_contents($pipes[1]), ' ', proc_close($p);`, "through cat 0"},
		{"proc_open env and cwd", `
			$p = proc_open(['sh', '-c', 'echo "$FOO $(pwd)"'], [1 => ['pipe', 'w']], $pipes, '/', ['FOO' => 'bar']);
			echo trim(stream_get_contents($pipes[1])); proc_close($p);`, "bar /"},
		{"proc_open socket", `
			$p = proc_open(['cat'], [['socket'], ['socket']], $pipes);
			fwrite($pipes[0], "line\n"); stream_socket_shutdown($pipes[0], STREAM_SHUT_WR);
			echo trim(fgets($pipes[1])); proc_close($p);`, "line"},
		{"proc_terminate", `
			$p = proc_open(['sleep', '10'], [], $pipes);
			proc_terminate($p);
			do { usleep(10000); $st = proc_get_status($p); } while ($st['running']);
			echo $st['termsig'];`, "15"},
		{"popen", `$h = popen('echo from popen; exit 2', 'r'); echo trim(fgets($h)), ' ', pclose($h);`, "from popen 2"},
		{"missing program", `var_dump(@proc_open(['/no/such/program'], [], $pipes));`, "bool(false)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, exit := runPHP(t, tt.code)
			if exit != 0 || strings.TrimSpace(out) != tt.want {
				t.Errorf("exit %d, got %q, want %q", exit, out, tt.want)
			}
		})
	}
}
