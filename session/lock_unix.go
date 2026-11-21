package session

import (
	"errors"
	"os"
	"syscall"
)

// tryLockExclusive attempts a non-blocking exclusive flock. A false result
// means another descriptor holds the lock.
func tryLockExclusive(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return false, nil
	default:
		return false, err
	}
}
