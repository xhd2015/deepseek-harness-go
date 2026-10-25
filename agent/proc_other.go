//go:build !unix

package agent

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
