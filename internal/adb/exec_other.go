//go:build !windows

package adb

import "os/exec"

func configureCommandWindow(cmd *exec.Cmd) {}
