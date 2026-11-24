//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | 0x00000200}
}

// Windows has no SIGTERM for a detached process; the daemon is killed and
// leaves its runtime directory to be cleaned on the next start.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
