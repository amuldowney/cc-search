package index

import (
	"errors"
	"fmt"
	"os"
	"time"
)

const lifecycleLockTimeout = 30 * time.Second

var errLifecycleLockBusy = errors.New("lifecycle lock is busy")

type lifecycleLock struct {
	file *os.File
}

func acquireLifecycleLock(indexPath string) (*lifecycleLock, error) {
	return acquireLifecycleLockWithTimeout(indexPath, lifecycleLockTimeout)
}

func acquireLifecycleLockNonblocking(indexPath string) (*lifecycleLock, error) {
	if err := lifecycleLockSupportError(); err != nil {
		return nil, fmt.Errorf("acquire lifecycle lock for index %q: %w", indexPath, err)
	}
	file, err := os.OpenFile(indexPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lifecycle lock for index %q: %w", indexPath, err)
	}
	if err := tryLifecycleLock(file); err != nil {
		_ = file.Close()
		if isLifecycleLockBusy(err) {
			return nil, fmt.Errorf("acquire lifecycle lock for index %q: %w", indexPath, errLifecycleLockBusy)
		}
		return nil, fmt.Errorf("acquire lifecycle lock for index %q: %w", indexPath, err)
	}
	return &lifecycleLock{file: file}, nil
}

func acquireLifecycleLockWithTimeout(indexPath string, timeout time.Duration) (*lifecycleLock, error) {
	if err := lifecycleLockSupportError(); err != nil {
		return nil, fmt.Errorf("acquire lifecycle lock for index %q: %w", indexPath, err)
	}

	lockPath := indexPath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lifecycle lock for index %q: %w", indexPath, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := tryLifecycleLock(file)
		if err == nil {
			return &lifecycleLock{file: file}, nil
		}
		if !isLifecycleLockBusy(err) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire lifecycle lock for index %q: %w", indexPath, err)
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = file.Close()
			return nil, fmt.Errorf("timed out waiting %s for index %q lifecycle lock",
				timeout, indexPath)
		}
		if remaining > 25*time.Millisecond {
			remaining = 25 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

func (l *lifecycleLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockLifecycleLock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
