package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockFileName is the advisory lock file inside ManifestDir. It serializes
// runs across processes (multiple developers / CI pipelines sharing one
// workspace): a later run either queues until the lock is released or, with
// the no-wait option, fails fast with an explicit "locked" conclusion.
const lockFileName = "run.lock"

// lockPollInterval is how often a queued run retries the lock.
const lockPollInterval = 15 * time.Millisecond

// fileLock holds the open file description the flock is attached to.
type fileLock struct {
	f *os.File
}

// acquireLock takes the exclusive workspace lock. By default it queues:
// while another process holds the lock it retries until the lock is free or
// ctx is done (which surfaces as an interruption). With noWait it returns
// errLocked immediately, giving an explicit "occupied" conclusion instead of
// queueing.
func acquireLock(ctx context.Context, dir string, noWait bool) (*fileLock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir for lock: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("acquire workspace lock: %w", err)
		}
		if noWait {
			f.Close()
			return nil, errLocked
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// release unlocks and closes the lock file. The lock file itself is left in
// place: removing it would race with another process that just opened it.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
