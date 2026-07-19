//go:build windows

package releasegate

import "os/exec"

func prepareManagedCommand(*exec.Cmd) {}

func terminateManagedCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killManagedCommand(cmd *exec.Cmd) error { return terminateManagedCommand(cmd) }
