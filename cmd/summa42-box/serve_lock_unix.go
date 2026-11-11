//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errServeRunning is returned when another serve holds the Collective.
var errServeRunning = errors.New("another summa42-box serve is already running on this Collective")

// lockServe takes the coordinator lock without waiting. The kernel drops the
// lock when the process ends, so a crashed serve never leaves it behind.
func lockServe(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("serve lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", errServeRunning, path)
		}
		return nil, fmt.Errorf("serve lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}
