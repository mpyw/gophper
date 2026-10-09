package gophper_test

import (
	"bytes"
	"context"
	"os"
	"runtime"
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
	ch := make(chan os.Signal, 1)
	out := &signalBuffer{ready: make(chan struct{})}
	go func() {
		select {
		case <-out.ready:
			ch <- sig
		case <-time.After(30 * time.Second):
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
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix signals")
	}
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
