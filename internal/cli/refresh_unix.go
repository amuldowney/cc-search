//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

func detachRefresh(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
