//declscope:namespace process

package hostproc

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

func TestProcessHostSignal(t *testing.T) {
	for _, tt := range []struct {
		linux int32
		want  os.Signal
		ok    bool
	}{
		{15, syscall.SIGTERM, true},
		{9, syscall.SIGKILL, true},
		{0, nil, false},
		{64, nil, false},
	} {
		got, ok := processHostSignal(tt.linux)
		if got != tt.want || ok != tt.ok {
			t.Errorf("processHostSignal(%d) = %v, %v, want %v, %v", tt.linux, got, ok, tt.want, tt.ok)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Linux numbers, whatever the host numbers them: SIGUSR1 is 30 on macOS.
	if got, ok := processHostSignal(10); !ok || got.String() != "user defined signal 1" {
		t.Errorf("processHostSignal(10) = %v, %v, want SIGUSR1", got, ok)
	}
	if n, ok := ProcessSignalNumber(processSignalByLinuxUnix[10]); !ok || n != 10 {
		t.Errorf("ProcessSignalNumber(SIGUSR1) = %d, %v, want 10", n, ok)
	}
}

func TestProcessExitSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	for _, tt := range []struct {
		script string
		want   int32
		ok     bool
	}{
		{"exit 0", 0, false},
		{"kill -TERM $$", 15, true},
		{"kill -USR2 $$", 12, true},
		// Not in the table: the host's own number, which Linux and macOS share.
		{"kill -SEGV $$", 11, true},
	} {
		cmd := exec.Command("/bin/sh", "-c", tt.script)
		_ = cmd.Run()
		got, ok := processExitSignal(cmd.ProcessState)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: got %d, %v, want %d, %v", tt.script, got, ok, tt.want, tt.ok)
		}
		if ok && processWaitStatus(cmd.ProcessState) != tt.want {
			t.Errorf("%s: wait status %#x, want %d", tt.script, processWaitStatus(cmd.ProcessState), tt.want)
		}
	}
}

// processOtherSignal is an os.Signal that is not a syscall.Signal.
type processOtherSignal struct{}

func (processOtherSignal) String() string { return "other" }
func (processOtherSignal) Signal()        {}

func TestProcessSignalNumber(t *testing.T) {
	for _, tt := range []struct {
		sig  os.Signal
		want int32
		ok   bool
	}{
		{syscall.SIGTERM, 15, true},
		{syscall.SIGSEGV, 0, false},
		{processOtherSignal{}, 0, false},
	} {
		if n, ok := ProcessSignalNumber(tt.sig); n != tt.want || ok != tt.ok {
			t.Errorf("ProcessSignalNumber(%v) = %d, %v, want %d, %v", tt.sig, n, ok, tt.want, tt.ok)
		}
	}
}
