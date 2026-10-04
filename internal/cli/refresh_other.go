//go:build !unix

package cli

import "os/exec"

func detachRefresh(command *exec.Cmd) {}
