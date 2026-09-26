// Package state persists generation metadata (a manifest) so repeated runs are
// reproducible and user modifications are detectable and explainable. It also
// owns the crash-recovery journal used by the engine for two-phase commits.
//
// All on-disk state lives under <root>/.scaffold/:
//
//	manifest.json      – last fully committed generation result
//	journal.json       – present only while a run is committing; its presence
//	                     proves a previous run was interrupted
//	backups/<safe>     – pre-run copies of overwritten outputs
//	tmp/<id>           – staging files for atomic rename
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ErrLocked indicates another engine run currently holds the workspace lock.
var ErrLocked = errors.New("workspace is locked by another scaffold-engine run")

const (
	stateDir  = ".scaffold"
	manifestN = "manifest.json"
	journalN  = "journal.json"
	lockN     = "run.lock"
	backupsD  = "backups"
	tmpD      = "tmp"

	// OutputMode is the fixed mode of every generated file, which makes
	// output reproducible across machines with different umasks.
	OutputMode os.FileMode = 0o644
	// DirMode is the fixed mode of directories created by the engine.
	DirMode os.FileMode = 0o755
)

// TargetState records what was produced for one target on the last committed
// run.
type TargetState struct {
	Name          string `json:"name"`
	OutputPath    string `json:"output_path"`
	ContentSHA256 string `json:"content_sha256"`
	// Rendered is the exact rendered content. Keeping it lets the engine
	// explain differences and restore files after an interrupted run.
	Rendered string `json:"rendered"`
	// Mode is recorded for reproducibility auditing.
	Mode uint32 `json:"mode"`
}

// Manifest is the persisted state of one fully committed plan execution.
type Manifest struct {
	PlanName string                 `json:"plan_name"`
	Version  int                    `json:"version"`
	Targets  map[string]TargetState `json:"targets"`
}

// Status classifies an on-disk output relative to the plan and manifest.
type Status string

const (
	// StatusMissing: the output does not exist and the engine never owned it.
	StatusMissing Status = "missing"
	// StatusUpToDate: the file exists and its digest equals the desired
	// digest (identical to the current plan).
	StatusUpToDate Status = "up_to_date"
	// StatusPlanChanged: the file is byte-identical to what the engine last
	// wrote, but the current plan renders different content; a safe rewrite
	// (engine-owned, untouched) is allowed.
	StatusPlanChanged Status = "plan_changed"
	// StatusUserModified: an engine-owned file was changed externally
	// (edited or deleted). Refused unless --force is given.
	StatusUserModified Status = "user_modified"
	// StatusForeign: the file exists but the engine never owned the path.
	// Refused unless --force is given.
	StatusForeign Status = "foreign"
)

// Decision is the explainable conclusion for one target.
type Decision struct {
	Target  string `json:"target"`
	Path    string `json:"path"`
	Status  Status `json:"status"`
	Reason  string `json:"reason"`
	Written bool   `json:"written"`
	Digest  string `json:"digest,omitempty"`
}

// JournalEntry describes one output changed during the in-flight run.
type JournalEntry struct {
	Name       string `json:"name"`
	OutputPath string `json:"output_path"`
	// Staged is the engine-dir-relative path of the staged temp file.
	Staged string `json:"staged"`
	// Backup is the engine-dir-relative pre-overwrite backup, empty when the
	// output did not previously exist.
	Backup string `json:"backup"`
	// WasTracked records whether the path belonged to the previous manifest
	// (true) or was freshly created this run (false).
	WasTracked bool `json:"was_tracked"`
}

// Journal is written before any output is moved into place and deleted only
// after the new manifest has been committed.
type Journal struct {
	Entries []JournalEntry `json:"entries"`
}

// Store reads and writes engine state inside a workspace.
type Store struct {
	rootDir string
}

// NewStore creates a Store rooted at rootDir.
func NewStore(rootDir string) *Store { return &Store{rootDir: rootDir} }

// Root reports the workspace root.
func (s *Store) Root() string { return s.rootDir }

func (s *Store) dir() string     { return filepath.Join(s.rootDir, stateDir) }
func (s *Store) backups() string { return filepath.Join(s.dir(), backupsD) }
func (s *Store) tmp() string     { return filepath.Join(s.dir(), tmpD) }

// ManifestPath returns the manifest file path.
func (s *Store) ManifestPath() string { return filepath.Join(s.dir(), manifestN) }

// JournalPath returns the journal file path.
func (s *Store) JournalPath() string { return filepath.Join(s.dir(), journalN) }

// Lock acquires an exclusive workspace lock so two engine instances cannot
// modify the same outputs concurrently. It returns a release function. A held
// lock by another process yields an error that callers can distinguish with
// errors.Is(err, ErrLocked).
func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(s.dir(), DirMode); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	path := filepath.Join(s.dir(), lockN)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, OutputMode)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	info := fmt.Sprintf("pid=%d\n", os.Getpid())
	_, _ = f.WriteString(info)
	_ = f.Close()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(path)
	}, nil
}

// BreakLock removes a stale lock (administrative recovery only).
func (s *Store) BreakLock() error {
	if err := os.Remove(filepath.Join(s.dir(), lockN)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Load reads the current manifest. A missing manifest is not an error and
// yields an empty manifest (first run).
func (s *Store) Load() (*Manifest, error) {
	raw, err := os.ReadFile(s.ManifestPath())
	if os.IsNotExist(err) {
		return &Manifest{Targets: map[string]TargetState{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Targets == nil {
		m.Targets = map[string]TargetState{}
	}
	return &m, nil
}

// Save atomically writes the manifest with deterministic key ordering.
func (s *Store) Save(m *Manifest) error {
	if err := os.MkdirAll(s.dir(), DirMode); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	body, err := json.MarshalIndent(canonicalManifest(m), "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	body = append(body, '\n')
	return writeFileAtomic(s.ManifestPath(), body, OutputMode)
}

// canonicalManifest returns a value with targets ordered by name, so the
// marshalled bytes do not depend on Go map iteration order.
func canonicalManifest(m *Manifest) *Manifest {
	keys := make([]string, 0, len(m.Targets))
	for k := range m.Targets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := &Manifest{PlanName: m.PlanName, Version: m.Version, Targets: map[string]TargetState{}}
	for _, k := range keys {
		out.Targets[k] = m.Targets[k]
	}
	return out
}

// StageFile writes content to a unique temp file under .scaffold/tmp and
// returns its path (absolute). The caller later installs it with Install.
func (s *Store) StageFile(name string, content []byte) (string, error) {
	if err := os.MkdirAll(s.tmp(), DirMode); err != nil {
		return "", fmt.Errorf("create tmp dir: %w", err)
	}
	f, err := os.CreateTemp(s.tmp(), "stage-*")
	if err != nil {
		return "", fmt.Errorf("create stage file: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("write stage file: %w", err)
	}
	if err := f.Chmod(OutputMode); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("chmod stage file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("close stage file: %w", err)
	}
	return tmp, nil
}

// BackupExisting copies absPath into the backups directory if it exists and
// returns the engine-dir-relative backup location ("" when nothing existed).
func (s *Store) BackupExisting(name, absPath string) (string, error) {
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("stat output: %w", err)
	}
	if err := os.MkdirAll(s.backups(), DirMode); err != nil {
		return "", fmt.Errorf("create backups dir: %w", err)
	}
	rel := "backups/" + backupName(name)
	dst := filepath.Join(s.dir(), filepath.FromSlash(rel))
	if err := copyFile(absPath, dst, OutputMode); err != nil {
		return "", err
	}
	return rel, nil
}

// Install moves the staged file to absPath, creating parent directories.
func (s *Store) Install(staged, absPath string) error {
	if err := os.MkdirAll(filepath.Dir(absPath), DirMode); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if err := os.Rename(staged, absPath); err != nil {
		// Cross-device rename: fall back to copy+remove.
		if err := copyFile(staged, absPath, OutputMode); err != nil {
			return err
		}
		return os.Remove(staged)
	}
	return nil
}

// RestoreBackup copies the engine-relative backup back over absPath.
func (s *Store) RestoreBackup(rel, absPath string) error {
	src := filepath.Join(s.dir(), filepath.FromSlash(rel))
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("backup missing: %w", err)
	}
	return copyFile(src, absPath, OutputMode)
}

// RemoveBackup deletes a backup file.
func (s *Store) RemoveBackup(rel string) {
	_ = os.Remove(filepath.Join(s.dir(), filepath.FromSlash(rel)))
}

// CleanupStaging removes the tmp and backups directories' leftovers.
func (s *Store) CleanupStaging() {
	_ = os.RemoveAll(s.tmp())
	_ = os.RemoveAll(s.backups())
}

// WriteJournal atomically persists the in-flight journal.
func (s *Store) WriteJournal(j *Journal) error {
	if err := os.MkdirAll(s.dir(), DirMode); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("encode journal: %w", err)
	}
	body = append(body, '\n')
	return writeFileAtomic(s.JournalPath(), body, OutputMode)
}

// ReadJournal loads the journal. A missing journal returns (nil, nil).
func (s *Store) ReadJournal() (*Journal, error) {
	raw, err := os.ReadFile(s.JournalPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	var j Journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("parse journal: %w", err)
	}
	return &j, nil
}

// RemoveJournal deletes the journal after a successful commit.
func (s *Store) RemoveJournal() error {
	if err := os.Remove(s.JournalPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove journal: %w", err)
	}
	return nil
}

// Recover undoes an interrupted run described by the journal: newly created
// outputs are deleted and overwritten outputs are restored from backup. It
// returns the number of entries recovered. It is idempotent.
// Recover undoes an interrupted run described by the journal: newly created
// outputs are deleted and overwritten outputs are restored from backup. It
// returns the number of entries recovered and is idempotent. Backup files are
// retained until the caller removes the journal and invokes CleanupStaging;
// this guarantees a recovery that fails part-way (or whose journal deletion
// fails) can still be retried.
func (s *Store) Recover(j *Journal) (int, error) {
	if j == nil {
		return 0, nil
	}
	var firstErr error
	for _, e := range j.Entries {
		abs := filepath.Join(s.rootDir, filepath.FromSlash(e.OutputPath))
		if e.Backup != "" {
			if err := s.RestoreBackup(e.Backup, abs); err != nil && firstErr == nil {
				firstErr = err
			}
		} else {
			// File did not exist before the run started.
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return len(j.Entries), firstErr
	}
	return len(j.Entries), nil
}

// Digest returns the lowercase hex SHA-256 of content.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// FileDigest returns the SHA-256 of an existing file.
func FileDigest(absPath string) (string, error) {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	return Digest(raw), nil
}

// backupName maps a target name to a safe backup file name.
func backupName(name string) string {
	var b []byte
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		b = []byte("target")
	}
	return string(b) + ".bak"
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), DirMode); err != nil {
		return fmt.Errorf("create dir for %s: %w", dst, err)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := writeFileAtomic(dst, raw, mode); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// writeFileAtomic writes via a temp file in the same directory plus rename so
// readers never observe a partially written file.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}
