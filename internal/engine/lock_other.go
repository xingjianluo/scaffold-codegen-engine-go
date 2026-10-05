//go:build windows

package engine

import (
	"context"
)

// LockFileName is the cross-process workspace lock file under .scaffold/.
const LockFileName = "lock"

// LockHolderName is the informational metadata file describing the holder.
const LockHolderName = "lockholder.json"

// KindWorkspaceBusy classifies a rejected run because another run holds the
// workspace lock exclusively.
const KindWorkspaceBusy = "workspace_busy"

type lockHolder struct {
	Mode      string `json:"mode"`
	PlanName  string `json:"planName,omitempty"`
	PID       int    `json:"pid"`
	Host      string `json:"host,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

type workspaceLock struct{}

func acquireWorkspaceLock(ctx context.Context, root string, exclusive, block bool, holder lockHolder) (*workspaceLock, error) {
	_ = ctx
	_ = root
	_ = exclusive
	_ = block
	_ = holder
	return &workspaceLock{}, nil
}

func (l *workspaceLock) release() error { return nil }

func tryReadWorkspaceLock(root string) (*workspaceLock, error) {
	_ = root
	return &workspaceLock{}, nil
}
