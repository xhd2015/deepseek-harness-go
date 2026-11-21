package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrAlreadyOwned reports that another process holds a session's write lease.
var ErrAlreadyOwned = errors.New("session: already owned by another process")

// LeaseFileName is the kernel lock file inside a session directory.
const LeaseFileName = "session.lock"

// lease is one held cross-process write lock. The arbiter is the kernel: a
// non-blocking exclusive lock on session.lock, released when the descriptor
// closes, including on process death. Readers never take it.
type lease struct {
	file *os.File
}

// acquireLease takes the session directory's exclusive write lease, creating
// the directory if needed.
func acquireLease(dir, id string) (*lease, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: create session directory: %w", err)
	}
	path := filepath.Join(dir, LeaseFileName)
	// Bounded retry: a releasing creator may have unlinked and recreated the
	// lock file, so the locked inode can stop being the file at the path.
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("session: open write lease: %w", err)
		}
		locked, err := tryLockExclusive(file)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("session: lock %s: %w", path, err)
		}
		if !locked {
			file.Close()
			return nil, fmt.Errorf("%w: session %q", ErrAlreadyOwned, id)
		}
		held, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("session: stat write lease: %w", err)
		}
		current, err := os.Stat(path)
		if err == nil && os.SameFile(held, current) {
			return &lease{file: file}, nil
		}
		// The locked inode is no longer the file at the lock path: start over
		// against whatever now stands there.
		file.Close()
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("session: stat write lease: %w", err)
		}
	}
	return nil, fmt.Errorf("%w: session %q", ErrAlreadyOwned, id)
}

// release closes the descriptor, which releases the kernel lock. The POSIX lock
// file is never removed: a durable inode keeps later lockers verifiable.
func (l *lease) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
