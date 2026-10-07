//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// detachProcess starts the command in its own session, so it keeps
// running after the terminal or the parent process goes away.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
