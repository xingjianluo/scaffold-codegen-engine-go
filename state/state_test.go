package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestRoundTripDeterministic(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	m := &Manifest{PlanName: "p", Version: 1, Targets: map[string]TargetState{
		"z": {Name: "z", OutputPath: "z", ContentSHA256: "1", Rendered: "Z"},
		"a": {Name: "a", OutputPath: "a", ContentSHA256: "2", Rendered: "A"},
	}}
	if err := s.Save(m); err != nil {
		t.Fatal(err)
	}
	body1, _ := os.ReadFile(s.ManifestPath())
	if err := s.Save(m); err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(s.ManifestPath())
	if string(body1) != string(body2) {
		t.Fatal("manifest bytes not deterministic across saves")
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanName != "p" || len(got.Targets) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// Missing manifest is a valid empty manifest.
	s2 := NewStore(t.TempDir())
	m2, err := s2.Load()
	if err != nil || len(m2.Targets) != 0 {
		t.Fatalf("empty manifest handling wrong: %+v %v", m2, err)
	}
}

func TestJournalRecoveryFreshAndBackup(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	root := s.Root()
	os.MkdirAll(filepath.Join(root, "out"), 0o755)
	// b pre-exists; simulate backup.
	os.WriteFile(filepath.Join(root, "out", "b.txt"), []byte("OLD\n"), 0o644)
	os.MkdirAll(s.backups(), 0o755)
	os.WriteFile(filepath.Join(s.dir(), "backups", "b.bak"), []byte("OLD\n"), 0o644)
	// a freshly installed (no backup); b overwritten.
	os.WriteFile(filepath.Join(root, "out", "a.txt"), []byte("NEW-A\n"), 0o644)
	os.WriteFile(filepath.Join(root, "out", "b.txt"), []byte("NEW-B\n"), 0o644)
	j := &Journal{Entries: []JournalEntry{
		{Name: "a", OutputPath: "out/a.txt"},
		{Name: "b", OutputPath: "out/b.txt", Backup: "backups/b.bak"},
	}}
	n, err := s.Recover(j)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("recovered %d", n)
	}
	if _, err := os.Stat(filepath.Join(root, "out", "a.txt")); !os.IsNotExist(err) {
		t.Fatal("fresh file must be deleted")
	}
	b, _ := os.ReadFile(filepath.Join(root, "out", "b.txt"))
	if string(b) != "OLD\n" {
		t.Fatalf("backup not restored: %q", b)
	}
	// Recovery is idempotent.
	if _, err := s.Recover(j); err != nil {
		t.Fatal(err)
	}
}

func TestLockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	release, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(); err != ErrLocked {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	release()
	release2, err := s.Lock()
	if err != nil {
		t.Fatalf("lock should be releasable: %v", err)
	}
	release2()
}

func TestDigestStable(t *testing.T) {
	d1 := Digest([]byte("hello"))
	d2 := Digest([]byte("hello"))
	if d1 != d2 {
		t.Fatal("digest unstable")
	}
	if d1 == Digest([]byte("hellx")) {
		t.Fatal("digest collision")
	}
}
