package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// BackupDirName is the root of run-local pre-run snapshots under .scaffold/.
const BackupDirName = "backup"

// backupIndexName records which paths were captured and whether they existed
// before the run, so a restore can run even from a fresh process.
const backupIndexName = "index.json"

// backupEntry records the pre-run situation of one path.
type backupEntry struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
}

// backup stores pre-run contents of paths that a run may change. Each backup
// is taken once, before the path is first written, so concurrent workers can
// never snapshot a half-written file.
type backup struct {
	root  string // output root
	dir   string // this run's backup directory
	mu    sync.Mutex
	seen  map[string]bool
	index map[string]backupEntry
}

// newBackup creates an empty backup for one run under
// <root>/.scaffold/backup/<runID>/.
func newBackup(root, runID string) (*backup, error) {
	dir := filepath.Join(root, ManifestDir, BackupDirName, runID)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	b := &backup{
		root:  root,
		dir:   dir,
		seen:  map[string]bool{},
		index: map[string]backupEntry{},
	}
	if err := b.persistIndex(); err != nil {
		return nil, err
	}
	return b, nil
}

// captureIfNeeded snapshots rel the first time it is requested. An existing
// file is copied byte-for-byte; a missing path is recorded as absent so a
// later restore deletes whatever the run created. Repeat calls are no-ops.
func (b *backup) captureIfNeeded(rel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[rel] {
		return nil
	}
	b.seen[rel] = true

	src := filepath.Join(b.root, rel)
	data, err := os.ReadFile(src)
	switch {
	case os.IsNotExist(err):
		b.index[rel] = backupEntry{Path: rel, Existed: false}
	default:
		if err != nil {
			return fmt.Errorf("backup read %s: %w", rel, err)
		}
		dst := filepath.Join(b.dir, "files", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("backup mkdir %s: %w", rel, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("backup write %s: %w", rel, err)
		}
		b.index[rel] = backupEntry{Path: rel, Existed: true}
	}
	return b.persistIndexLocked()
}

// restore puts every captured path back to its pre-run bytes, deleting files
// that did not exist before the run, then prunes directories left empty. It
// returns every error encountered (best-effort, never short-circuited).
func (b *backup) restore() []error {
	b.mu.Lock()
	entries := make([]backupEntry, 0, len(b.index))
	for _, e := range b.index {
		entries = append(entries, e)
	}
	b.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	var errs []error
	var dirs []string
	for _, e := range entries {
		full := filepath.Join(b.root, filepath.FromSlash(e.Path))
		if !e.Existed {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove created file %s: %w", e.Path, err))
			}
			dirs = append(dirs, filepath.Dir(full))
			continue
		}
		src := filepath.Join(b.dir, "files", filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("restore mkdir %s: %w", e.Path, err))
			continue
		}
		if err := copyFile(src, full); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", e.Path, err))
		}
	}
	pruneEmptyDirs(b.root, dirs)
	return errs
}

// remove deletes this run's backup directory.
func (b *backup) remove() error {
	return os.RemoveAll(b.dir)
}

// path returns the backup directory (used for stale-run detection).
func (b *backup) path() string { return b.dir }

func (b *backup) persistIndex() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.persistIndexLocked()
}

// persistIndexLocked writes the index atomically. Caller holds b.mu.
func (b *backup) persistIndexLocked() error {
	entries := make([]backupEntry, 0, len(b.index))
	for _, e := range b.index {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup index: %w", err)
	}
	if err := writeStateFile(filepath.Join(b.dir, backupIndexName), data); err != nil {
		return fmt.Errorf("write backup index: %w", err)
	}
	return nil
}

// listStaleBackups returns run backup directories left behind by earlier
// processes (normally zero: a finished run deletes its backup).
func listStaleBackups(root string) ([]string, error) {
	base := filepath.Join(root, ManifestDir, BackupDirName)
	items, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, it := range items {
		if it.IsDir() {
			out = append(out, filepath.Join(base, it.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// backupFromDir opens an existing backup directory for restore (e.g. left by
// a process that was killed).
func backupFromDir(root, dir string) *backup {
	b := &backup{root: root, dir: dir, seen: map[string]bool{}, index: map[string]backupEntry{}}
	data, err := os.ReadFile(filepath.Join(dir, backupIndexName))
	if err == nil {
		var entries []backupEntry
		if json.Unmarshal(data, &entries) == nil {
			for _, e := range entries {
				b.index[e.Path] = e
				b.seen[e.Path] = true
			}
		}
	}
	return b
}

// copyFile copies a single file's bytes and mode.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pruneEmptyDirs removes the given directories (and parents up to root) while
// they are empty, ignoring "not empty" errors.
func pruneEmptyDirs(root string, dirs []string) {
	seen := map[string]bool{}
	candidates := make([]string, 0, len(dirs))
	for _, d := range dirs {
		for d != root && d != "." && d != "/" {
			if seen[d] {
				break
			}
			seen[d] = true
			candidates = append(candidates, d)
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	for _, d := range candidates {
		if err := os.Remove(d); err != nil {
			break // not empty or otherwise non-removable: stop walking up
		}
	}
}
