//go:build windows

package session

import "os"

// tryLockExclusive reports exclusive access on Windows through the process-wide
// session registry: this store does not implement the DSH named-semaphore lease,
// so two dsh-go processes must not share one session root.
func tryLockExclusive(file *os.File) (bool, error) {
	return true, nil
}
