//go:build windows

package service

import (
	"os"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

func interruptProcess(int) error {
	return domain.Errorf(domain.ErrCapabilityUnavailable, "cancelling a session from another process is not supported on Windows yet; press Ctrl+C in the window running it")
}
