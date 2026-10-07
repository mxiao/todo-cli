package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ptsName unlocks and names the slave side of a Linux pty master.
func ptsName(fd int) (string, error) {
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return "", err
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}
