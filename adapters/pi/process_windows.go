//go:build windows

package pi

import "os/exec"

func preparePiCommand(*exec.Cmd) {}

func cancelPiCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
