//go:build unix

package gophper_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/mpyw/gophper"
)

// runSignals runs code and sends sig once the script prints "ready".
func runSignals(t *testing.T, code string, sig os.Signal) (string, int) {
	t.Helper()
	return runSignalsEvery(t, code, sig, 0)
}

// runSignalsEvery is runSignals, but with every above zero, it sends sig
// again at that interval until the run ends. A test whose script must be
// blocked when the signal comes needs it: "ready" is printed just before
// the script blocks, so the first signal may arrive too early.
func runSignalsEvery(t *testing.T, code string, sig os.Signal, every time.Duration) (string, int) {
	t.Helper()
	ch := make(chan os.Signal, 1)
	out := &signalBuffer{ready: make(chan struct{})}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-out.ready:
		case <-time.After(30 * time.Second):
			return
		}
		for {
			select {
			case ch <- sig:
			default:
			}
			if every <= 0 {
				return
			}
			select {
			case <-time.After(every):
			case <-done:
				return
			}
		}
	}()
	exit, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args:    []string{"-r", code},
		Stdout:  out,
		Stderr:  out,
		FS:      wazero.NewFSConfig(),
		Signals: ch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), exit
}

func TestSignal(t *testing.T) {
	t.Run("handler", func(t *testing.T) {
		out, exit := runSignals(t, `
			pcntl_async_signals(true);
			pcntl_signal(SIGUSR1, function ($n) { echo "got $n\n"; exit(0); });
			echo "ready\n";
			sleep(10);
			echo "not interrupted\n";`, syscall.SIGUSR1)
		if exit != 0 || out != "ready\ngot 10\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
	t.Run("default action", func(t *testing.T) {
		out, exit := runSignals(t, `echo "ready\n"; sleep(10); echo "not interrupted\n";`, syscall.SIGTERM)
		if exit != 143 || out != "ready\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
	t.Run("ignored", func(t *testing.T) {
		out, exit := runSignals(t, `pcntl_signal(SIGTERM, SIG_IGN); echo "ready\n"; usleep(300000); echo "done\n";`, syscall.SIGTERM)
		if exit != 0 || out != "ready\ndone\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
	t.Run("alarm and self", func(t *testing.T) {
		out, exit := runSignals(t, `
			pcntl_async_signals(true);
			pcntl_signal(SIGALRM, fn($n) => print("alarm\n"));
			pcntl_signal(SIGUSR2, fn($n) => print("self\n"));
			posix_kill(posix_getpid(), SIGUSR2);
			pcntl_sigprocmask(SIG_BLOCK, [SIGUSR2]);
			posix_kill(posix_getpid(), SIGUSR2);
			echo "blocked\n";
			pcntl_sigprocmask(SIG_UNBLOCK, [SIGUSR2]);
			pcntl_alarm(1);
			sleep(5);
			echo "ready\n";`, syscall.SIGWINCH)
		if want := "self\nblocked\nself\nalarm\nready\n"; exit != 0 || out != want {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
	t.Run("sigtimedwait timeout", func(t *testing.T) {
		start := time.Now()
		out, exit := runSignals(t, `
			pcntl_sigprocmask(SIG_BLOCK, [SIGUSR1]);
			var_export(pcntl_sigtimedwait([SIGUSR1], $info, 0, 50000000)); echo "\nready\n";`, syscall.SIGWINCH)
		if exit != 0 || out != "false\nready\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
		if d := time.Since(start); d < 50*time.Millisecond {
			t.Errorf("returned after %v, before the timeout", d)
		}
	})
	t.Run("sigtimedwait delivery", func(t *testing.T) {
		out, exit := runSignals(t, `
			pcntl_sigprocmask(SIG_BLOCK, [SIGUSR1]);
			echo "ready\n";
			echo pcntl_sigtimedwait([SIGUSR1], $info, 10), ' ', $info['signo'], "\n";`, syscall.SIGUSR1)
		if exit != 0 || out != "ready\n10 10\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
	t.Run("sigwaitinfo delivery", func(t *testing.T) {
		out, exit := runSignals(t, `
			pcntl_sigprocmask(SIG_BLOCK, [SIGUSR2]);
			echo "ready\n";
			echo pcntl_sigwaitinfo([SIGUSR2], $info), "\n";`, syscall.SIGUSR2)
		if exit != 0 || out != "ready\n12\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
	t.Run("sigwaitinfo interrupted", func(t *testing.T) {
		// Another signal, with a handler, cuts the wait short. It is sent
		// until the run ends: one that came before the wait began would run
		// the handler and leave the wait with nothing to end it.
		out, exit := runSignalsEvery(t, `
			pcntl_async_signals(true);
			pcntl_signal(SIGUSR2, function ($n) { $GLOBALS["handled"] = $n; });
			pcntl_sigprocmask(SIG_BLOCK, [SIGUSR1]);
			echo "ready\n";
			var_export(@pcntl_sigwaitinfo([SIGUSR1], $info));
			echo ' ', pcntl_strerror(pcntl_get_last_error()), ' handled ', $GLOBALS["handled"] ?? 0, "\n";`, syscall.SIGUSR2, 100*time.Millisecond)
		if exit != 0 || out != "ready\nfalse Interrupted system call handled 12\n" {
			t.Errorf("exit %d\n%s", exit, out)
		}
	})
}

// signalBuffer closes ready once "ready" is written.
type signalBuffer struct {
	bytes.Buffer
	ready chan struct{}
	once  bool
}

func (b *signalBuffer) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	if !b.once && strings.Contains(b.String(), "ready") {
		b.once = true
		close(b.ready)
	}
	return n, err
}

// TestSignalSleepInHandler sleeps in a handler while the next signal is
// pending. Each sleep sleeps, as native PHP blocks signals in a handler:
// returning at once for the pending signal spun the loop millions of times.
func TestSignalSleepInHandler(t *testing.T) {
	out, code := runPHP(t, `
		pcntl_async_signals(true);
		pcntl_signal(SIGALRM, function () {
			pcntl_alarm(1);
			$n = 0;
			for ($end = microtime(true) + 2; microtime(true) < $end; $n++) {
				usleep(100000);
			}
			echo $n < 100 ? "slept\n" : "spun $n times\n";
			exit(0);
		});
		pcntl_alarm(1);
		sleep(10);
		echo "not interrupted\n";`)
	if code != 0 || out != "slept\n" {
		t.Errorf("exit %d: %q", code, out)
	}
}

// TestSignalTerminatesStuckRun sends SIGTERM while PHP is stuck in a Go
// function, where it cannot act on it. After the grace period, the host
// ends the run, which reports 128 plus the signal, as a shell does.
func TestSignalTerminatesStuckRun(t *testing.T) {
	ch := make(chan os.Signal, 1)
	ready := make(chan struct{})
	go func() {
		<-ready
		ch <- syscall.SIGTERM
	}()
	start := time.Now()
	code, err := newTestEngine(t).RunCLI(context.Background(), gophper.Options{
		Args: []string{"-r", `go_block();`},
		Functions: map[string]gophper.Function{
			"go_block": func(ctx context.Context, _ []any) (any, error) {
				close(ready)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		Signals: ch,
	})
	if err != nil || code != 128+15 {
		t.Errorf("exit %d, %v", code, err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("took %s", d)
	}
}
