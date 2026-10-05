package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// journalName is the run-state file inside ManifestDir. It records the last
// run's phase and per-target results so the state can be inspected at any
// time, even after a failed or interrupted run.
const journalName = "run-state.json"

// journalVersion is the on-disk run-state schema version.
const journalVersion = 1

// Run phases recorded in the journal.
const (
	// PhaseRunning: a run started but has not finished yet. If no process
	// holds the lock while the journal says running, the last run was
	// interrupted (killed) before it could finalize.
	PhaseRunning = "running"
	// PhaseCommitted: the last run finished successfully and committed.
	PhaseCommitted = "committed"
	// PhaseRolledBack: the last run failed or was interrupted and every
	// artifact it created or overwrote was rolled back.
	PhaseRolledBack = "rolled_back"
)

// RunState is the persisted outcome of the most recent run.
type RunState struct {
	Version    int            `json:"version"`
	RunID      string         `json:"runID"`
	PlanName   string         `json:"planName,omitempty"`
	Phase      string         `json:"phase"`
	StartedAt  string         `json:"startedAt"`
	FinishedAt string         `json:"finishedAt,omitempty"`
	Success    bool           `json:"success"`
	Error      *ClassifiedErr `json:"error,omitempty"`
	Results    []Result       `json:"results,omitempty"`
}

// loadJournal reads the run-state file. A missing file yields (nil, nil);
// a corrupt file is an explicit error.
func loadJournal(dir string) (*RunState, error) {
	data, err := os.ReadFile(filepath.Join(dir, journalName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run state: %w", err)
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse run state %s: %w", journalName, err)
	}
	return &st, nil
}

// saveJournal writes the run-state atomically (temp file + fsync + rename),
// the same crash-safety discipline as the manifest.
func saveJournal(dir string, st *RunState) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run state: %w", err)
	}
	final := filepath.Join(dir, journalName)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp run state: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write temp run state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync temp run state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit run state: %w", err)
	}
	return nil
}
