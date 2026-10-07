//go:build unix

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type signal = syscall.Signal

const (
	sigStop = syscall.SIGSTOP
	sigCont = syscall.SIGCONT
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// setProcessGroup starts the command as the leader of a new process group.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup signals the process group led by p.
func signalGroup(p *os.Process, sig signal) error {
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// processAlive reports whether a process with this pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
