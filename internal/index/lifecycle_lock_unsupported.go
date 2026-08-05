//go:build !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd

package index

import (
	"errors"
	"os"
)

var errUnsupportedLifecycleLock = errors.New("lifecycle locks require a supported Unix build")

func lifecycleLockSupportError() error {
	return errUnsupportedLifecycleLock
}

func tryLifecycleLock(*os.File) error {
	return errUnsupportedLifecycleLock
}

func isLifecycleLockBusy(error) bool {
	return false
}

func unlockLifecycleLock(*os.File) error {
	return nil
}
