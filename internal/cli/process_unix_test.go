//go:build !windows

package cli

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCLITimeoutKillsChildProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	start := time.Now()
	res := runCLI([]string{"sh", "-c", "sleep 30 & echo $!; wait"}, nil, 0, 1)
	elapsed := time.Since(start)
	if res.ExitCode != 124 {
		t.Fatalf("exit code = %d, want timeout 124; stderr=%q", res.ExitCode, res.Stderr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout returned after %s, want prompt process-group cleanup", elapsed.Round(time.Millisecond))
	}

	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		t.Fatalf("child pid missing from stdout: %q", res.Stdout)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parse child pid %q: %v", fields[0], err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child process %d survived parent timeout", pid)
}

func TestRunCLIContextCancelKillsChildProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := runCLIContext(ctx, []string{"sh", "-c", "sleep 30 & echo $!; wait"}, nil, 0, 0)
	elapsed := time.Since(start)
	if res.ExitCode != 124 {
		t.Fatalf("exit code = %d, want canceled 124; stderr=%q", res.ExitCode, res.Stderr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancel returned after %s, want prompt process-group cleanup", elapsed.Round(time.Millisecond))
	}

	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		t.Fatalf("child pid missing from stdout: %q", res.Stdout)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parse child pid %q: %v", fields[0], err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child process %d survived context cancellation", pid)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
