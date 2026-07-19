//go:build windows

package cli

import "os/exec"

func prepareCommand(cmd *exec.Cmd) {}

func cancelCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
