//go:build unix

package tool

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A cancelled request must not leave the command it started running. The command
// runs in its own process group, so cancelling kills the tree rather than only the
// shell that was started directly - without that, a `sh -c` wrapper would leave its
// children behind and the request would be "cancelled" while the work continued.
//
// This runs on the Linux and macOS CI legs. The wait below polls a real observable
// (the child printing its pid) with a deadline rather than sleeping for a guessed
// duration; there is no barrier to wait on for a process that has not been started
// by the test itself.
func TestCancellingACommandKillsItsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh is unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The shell backgrounds a long sleep and prints its pid, so the test knows
	// which process must not survive.
	command := exec.Command("sh", "-c", "sleep 30 & echo $!; wait")
	// The command writes to this buffer from its own goroutine while the test polls
	// it, so it is guarded: the first version used a bare strings.Builder and the
	// race detector found it. That is a race in the test, not in the code under
	// test, and it is reported here rather than hidden by a sleep.
	var output guardedBuffer
	command.Stdout = &output
	done := make(chan error, 1)
	go func() { done <- runCommandTree(ctx, command) }()

	childPID := 0
	deadline := time.Now().Add(5 * time.Second)
	for childPID == 0 && time.Now().Before(deadline) {
		fields := strings.Fields(output.String())
		if len(fields) > 0 {
			if pid, err := strconv.Atoi(fields[0]); err == nil {
				childPID = pid
			}
		}
		if childPID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if childPID == 0 {
		cancel()
		<-done
		t.Fatalf("the fixture never started its child: %q", output.String())
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation to be reported", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not return after its context was cancelled")
	}

	// The grandchild is gone. A just-killed process can still be visible as a
	// zombie until it is reaped, so the check is given a bounded grace period
	// instead of a single reading.
	gone := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(childPID, 0); err != nil {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("the child %d survived the cancellation", childPID)
	}
}

// guardedBuffer is a writer that can be read while another goroutine writes it.
type guardedBuffer struct {
	mu    sync.Mutex
	value strings.Builder
}

func (b *guardedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.Write(p)
}

func (b *guardedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.String()
}

// A command that finishes before its context is cancelled is reported normally, so
// the cancellation path is not taken for ordinary work.
func TestAFinishedCommandIsNotReportedAsCancelled(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh is unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.Command("sh", "-c", "exit 0")
	if err := runCommandTree(ctx, command); err != nil {
		t.Fatalf("err = %v", err)
	}
	// A failing command reports its own error, which is not the cancellation.
	failing := exec.Command("sh", "-c", "exit 3")
	err := runCommandTree(ctx, failing)
	if err == nil {
		t.Fatal("a failing command reported success")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("a failing command was reported as cancelled: %v", err)
	}
}
