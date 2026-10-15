// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package secret

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the command in its own process group and makes
// cancellation kill the whole group, including background children.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
