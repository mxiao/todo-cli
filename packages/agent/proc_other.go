//go:build !unix

package agent

import (
	"fmt"
	"os"
	"os/exec"
)

type signal int

const (
	sigStop signal = iota
	sigCont
	sigTerm
	sigKill
)

func setProcessGroup(*exec.Cmd) {}

func signalGroup(p *os.Process, sig signal) error {
	switch sig {
	case sigKill, sigTerm:
		return p.Kill()
	case sigCont:
		return nil
	}
	return fmt.Errorf("%w: pausing processes needs macOS or another unix system", ErrUnsupported)
}

func processAlive(pid int) bool { return pid > 0 }
