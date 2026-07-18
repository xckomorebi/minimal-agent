//go:build windows

package main

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {
	// Windows doesn't support Unix process groups.
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
