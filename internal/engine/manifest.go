package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// manifestVersion is the current on-disk manifest schema version.
const manifestVersion = 1

// Manifest records the committed outputs of successful runs.
type Manifest struct {
	Version int                  `json:"version"`
	Entries map[string]FileEntry `json:"entries"`
}

// FileEntry is the recorded state of one generated path. The entry is keyed by
// path; Content is embedded so digest comparison never needs the file to be
// present and so reports are self-explanatory.
type FileEntry struct {
	Target  string `json:"target"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// newEmptyManifest returns a manifest ready for use.
func newEmptyManifest() Manifest {
	return Manifest{Version: manifestVersion, Entries: map[string]FileEntry{}}
}

// digest returns the lowercase hex SHA-256 of s.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// loadManifest reads the manifest from dir/<name>. A missing file yields an
// empty manifest with no error; a corrupt file is an explicit error.
func loadManifest(dir, name string) (Manifest, error) {
	m := newEmptyManifest()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse manifest %s: %w", name, err)
	}
	if m.Entries == nil {
		m.Entries = map[string]FileEntry{}
	}
	return m, nil
}

// writeStateFile writes data to finalPath atomically: a temp file in the same
// directory is fsynced and then renamed over the destination, so a crash can
// leave only the old or the new bytes, never a half-written state file.
func writeStateFile(finalPath string, data []byte) error {
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := finalPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// saveManifest writes the manifest atomically: write a temp file in the same
// directory, fsync it, then rename over the destination. A crash can leave
// either the old file or the new one, never a half-written file.
func saveManifest(dir, name string, m Manifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	final := filepath.Join(dir, name)
	if err := writeStateFile(final, data); err != nil {
		return fmt.Errorf("commit manifest: %w", err)
	}
	return nil
}
