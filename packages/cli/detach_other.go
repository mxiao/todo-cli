//go:build !unix

package cli

import "os/exec"

func detachProcess(*exec.Cmd) {}
