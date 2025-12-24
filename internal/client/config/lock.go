package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockInfo contains information about the lock holder.
type LockInfo struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// ErrAlreadyRunning indicates another instance is running.
var ErrAlreadyRunning = errors.New("another gopublic instance is already running")

// LockFilePath returns the path to the lock file.
func LockFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gopublic.lock"), nil
}

// AcquireLock tries to acquire the lock file.
// Returns nil if lock acquired, ErrAlreadyRunning if another instance is running.
func AcquireLock() error {
	lockPath, err := LockFilePath()
	if err != nil {
		return err
	}

	// Check if lock file exists and process is running
	if info, err := readLockFile(lockPath); err == nil {
		if isProcessRunning(info.PID) {
			return fmt.Errorf("%w (PID: %d)", ErrAlreadyRunning, info.PID)
		}
		// Stale lock file - process not running, safe to remove
		os.Remove(lockPath)
	}

	// Create lock file with current PID
	return writeLockFile(lockPath)
}

// ReleaseLock removes the lock file.
func ReleaseLock() error {
	lockPath, err := LockFilePath()
	if err != nil {
		return err
	}

	// Only remove if it's our lock (same PID)
	if info, err := readLockFile(lockPath); err == nil {
		if info.PID == os.Getpid() {
			return os.Remove(lockPath)
		}
	}
	return nil
}

// ForceReleaseLock forcibly removes the lock file regardless of owner.
func ForceReleaseLock() error {
	lockPath, err := LockFilePath()
	if err != nil {
		return err
	}
	// Ignore error if file doesn't exist
	os.Remove(lockPath)
	return nil
}

func readLockFile(path string) (*LockInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func writeLockFile(path string) error {
	info := LockInfo{
		PID:       os.Getpid(),
		StartedAt: time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Send signal 0 to check if process exists.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}
