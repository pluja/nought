// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package secret

import "os/exec"

// setProcessGroup is a no-op on Windows; WaitDelay still bounds the wait.
func setProcessGroup(*exec.Cmd) {}
