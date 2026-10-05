//go:build !windows

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockFileName is the cross-process workspace lock file under .scaffold/.
const LockFileName = "lock"

// LockHolderName is the informational metadata file describing the current
// lock holder. It is kept separate from the flock file so it can be replaced
// atomically without changing the locked inode.
const LockHolderName = "lockholder.json"

// KindWorkspaceBusy classifies a rejected run because another run holds the
// workspace lock exclusively.
const KindWorkspaceBusy = "workspace_busy"

// lockHolder is the informational metadata about who holds the lock.
type lockHolder struct {
	Mode      string `json:"mode"` // "run" (exclusive) or "read" (shared)
	PlanName  string `json:"planName,omitempty"`
	PID       int    `json:"pid"`
	Host      string `json:"host,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

// workspaceLock is an OS-level advisory file lock (flock) shared across
// processes. The zero value must not be used; acquire it with
// acquireWorkspaceLock.
type workspaceLock struct {
	f         *os.File
	dir       string
	exclusive bool
}

// lockPollInterval bounds how long a blocking lock acquisition waits between
// non-blocking attempts while still observing context cancellation.
const lockPollInterval = 20 * time.Millisecond

// acquireWorkspaceLock takes an flock on .scaffold/lock inside root. When
// exclusive is true the lock conflicts with any other holder; otherwise it is
// a shared lock. When block is true the call waits (until ctx is done); when
// false it returns immediately with a KindWorkspaceBusy-classified error if
// the lock is held. holder is informational metadata about the caller.
func acquireWorkspaceLock(ctx context.Context, root string, exclusive, block bool, holder lockHolder) (*workspaceLock, error) {
	dir := filepath.Join(root, ManifestDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}

	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		lockErr := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("acquire workspace lock: %w", lockErr)
		}
		if !block {
			current := readLockHolder(dir)
			f.Close()
			return nil, busyError(current)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}

	lock := &workspaceLock{f: f, dir: dir, exclusive: exclusive}
	// Publish holder metadata best-effort; locking itself already succeeded.
	if data, mErr := json.MarshalIndent(holder, "", "  "); mErr == nil {
		_ = writeStateFile(filepath.Join(dir, LockHolderName), data)
	}
	return lock, nil
}

// release frees the flock and closes the file. Only the exclusive holder
// created lockholder.json; shared (read) locks never own that metadata and so
// never remove it. Safe to call once.
func (l *workspaceLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if l.exclusive {
		_ = os.Remove(filepath.Join(l.dir, LockHolderName))
	}
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// tryReadWorkspaceLock takes a non-blocking shared flock WITHOUT creating any
// file: it opens the existing lock read-only, so a preview or status query on
// a workspace that was never run touches nothing. Returns (nil, nil) when no
// lock file exists (nobody can be holding it); returns a
// KindWorkspaceBusy-classified error when an exclusive run is in progress.
func tryReadWorkspaceLock(root string) (*workspaceLock, error) {
	dir := filepath.Join(root, ManifestDir)
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); lockErr != nil {
		f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, busyError(readLockHolder(dir))
		}
		return nil, fmt.Errorf("acquire workspace lock: %w", lockErr)
	}
	return &workspaceLock{f: f, dir: dir, exclusive: false}, nil
}

// busyError builds the classified "someone else holds the workspace" error,
// enriching it with whatever holder metadata is currently published.
func busyError(current *lockHolder) error {
	msg := "workspace is locked by another run; refusing to start so concurrent runs cannot interleave state"
	if current != nil {
		who := current.Host
		if current.PID != 0 {
			who = fmt.Sprintf("%s(pid=%d)", who, current.PID)
		}
		msg += fmt.Sprintf("; holder: mode=%s plan=%q since=%s %s",
			current.Mode, current.PlanName, current.StartedAt, who)
	}
	msg += "; retry later or pass -wait-lock to queue behind the current holder"
	return &ClassifiedErr{Kind: KindWorkspaceBusy, Message: msg}
}

// readLockHolder reads the informational lock-holder file. A missing or
// corrupt file yields nil without error.
func readLockHolder(dir string) *lockHolder {
	data, err := os.ReadFile(filepath.Join(dir, LockHolderName))
	if err != nil {
		return nil
	}
	var h lockHolder
	if err := json.Unmarshal(data, &h); err != nil {
		return nil
	}
	return &h
}
