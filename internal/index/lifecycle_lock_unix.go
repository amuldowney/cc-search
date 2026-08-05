//go:build darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd

package index

import (
	"os"
	"syscall"
)

func lifecycleLockSupportError() error {
	return nil
}

func tryLifecycleLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func isLifecycleLockBusy(err error) bool {
	return err == syscall.EWOULDBLOCK || err == syscall.EAGAIN
}

func unlockLifecycleLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
