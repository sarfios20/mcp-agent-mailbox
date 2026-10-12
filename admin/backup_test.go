package admin

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestCreateBackupIsValidAndChecksummed catches lost live WAL data, invalid sidecars,
// incorrect UTC filenames and snapshot artifacts left after a successful backup.
func TestCreateBackupIsValidAndChecksummed(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "source.sqlite")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE messages (value TEXT)",
		"INSERT INTO messages VALUES ('hello')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	backupDir := filepath.Join(root, "backups")
	clock := func() time.Time { return time.Date(2026, 9, 28, 7, 15, 0, 0, time.FixedZone("local", 2*60*60)) }
	result, err := createBackup(database, backupDir, clock)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(backupDir, "mailbox-20260928T051500Z.sqlite")
	if result.Snapshot != expected || result.Kept != 1 || result.Deleted == nil || len(result.Deleted) != 0 {
		t.Fatalf("unexpected backup result: %+v", result)
	}
	if err := verifySnapshot(result.Snapshot); err != nil {
		t.Fatal(err)
	}
	digest, err := backupDigest(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := os.ReadFile(checksumPath(result.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if string(sidecar) != digest+"  "+filepath.Base(expected)+"\n" {
		t.Fatalf("sidecar is not in sha256sum format: %q", sidecar)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("backup left unexpected files: %v", entries)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("backup permissions: %s has %o", entry.Name(), info.Mode().Perm())
		}
	}
	copyDB, err := openBackupDatabase(result.Snapshot, true)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var value string
	if err := copyDB.QueryRow("SELECT value FROM messages").Scan(&value); err != nil || value != "hello" {
		t.Fatalf("snapshot lost WAL message: value=%q error=%v", value, err)
	}
}

// TestSparseRetentionAndMalformedPreservation catches bucket selection mistakes
// and deletion of unrelated, invalidly named, corrupt, unverified or symlinked files.
func TestSparseRetentionAndMalformedPreservation(t *testing.T) {
	root := t.TempDir()
	stamps := []string{
		"20260928T051500Z", "20260928T041500Z", "20260927T051500Z",
		"20260926T051500Z", "20260925T051500Z", "20260914T051500Z",
		"20260831T051500Z", "20260731T051500Z", "20260630T051500Z",
		"20250531T051500Z",
	}
	for _, stamp := range stamps {
		writeBackupFixture(t, root, stamp)
	}
	preserved := map[string][]byte{
		"mailbox-20240101T000000Z.sqlite": []byte("unverified"),
		"mailbox-20240201T000000Z.sqlite": []byte("missing sidecar"),
		"mailbox-20241301T000000Z.sqlite": []byte("invalid date"),
		"notes.txt":                       []byte("keep me"),
	}
	for name, content := range preserved {
		if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	malformed := filepath.Join(root, "mailbox-20240101T000000Z.sqlite")
	writeFixtureChecksum(t, malformed)
	preserved[filepath.Base(checksumPath(malformed))], _ = os.ReadFile(checksumPath(malformed))
	badChecksum := writeBackupFixture(t, root, "20240301T000000Z")
	if err := os.WriteFile(checksumPath(badChecksum), []byte("wrong checksum\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{badChecksum, checksumPath(badChecksum)} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		preserved[filepath.Base(path)] = content
	}
	link := filepath.Join(root, "mailbox-20240401T000000Z.sqlite")
	if err := os.Symlink(filepath.Join(root, "mailbox-20260928T051500Z.sqlite"), link); err != nil {
		t.Fatal(err)
	}
	writeFixtureChecksum(t, link)
	kept, deleted, err := pruneBackups(root)
	if err != nil {
		t.Fatal(err)
	}
	expectedDeleted := []string{"mailbox-20260914T051500Z.sqlite", "mailbox-20260925T051500Z.sqlite", "mailbox-20260928T041500Z.sqlite"}
	sort.Strings(deleted)
	if kept != 7 || !reflect.DeepEqual(deleted, expectedDeleted) {
		t.Fatalf("unexpected retention: kept=%d deleted=%v", kept, deleted)
	}
	for _, stamp := range stamps {
		name := "mailbox-" + stamp + ".sqlite"
		removed := stamp == "20260914T051500Z" || stamp == "20260925T051500Z" || stamp == "20260928T041500Z"
		for _, path := range []string{filepath.Join(root, name), checksumPath(filepath.Join(root, name))} {
			_, err := os.Stat(path)
			if removed && !os.IsNotExist(err) || !removed && err != nil {
				t.Fatalf("unexpected retained file state for %s: %v", path, err)
			}
		}
	}
	for name, content := range preserved {
		actual, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !reflect.DeepEqual(actual, content) {
			t.Fatalf("preserved file changed: %s: %v", name, err)
		}
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("snapshot symlink was not preserved: %v", err)
	}
	if _, err := os.Stat(checksumPath(link)); err != nil {
		t.Fatal(err)
	}
}

// TestFailedBackupDoesNotPrune catches pruning before a new snapshot verifies,
// including a source failure after temporary output has been created.
func TestFailedBackupDoesNotPrune(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	if err := os.Mkdir(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"20260928T051500Z", "20260927T051500Z", "20260926T051500Z", "20260925T051500Z"} {
		writeBackupFixture(t, backupDir, stamp)
	}
	before := make(map[string][]byte)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(backupDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		before[entry.Name()] = data
	}
	corrupt := filepath.Join(root, "corrupt.sqlite")
	if err := os.WriteFile(corrupt, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 9, 29, 5, 15, 0, 0, time.UTC) }
	for _, source := range []string{filepath.Join(root, "missing.sqlite"), corrupt} {
		if _, err := createBackup(source, backupDir, clock); err == nil {
			t.Fatalf("backup succeeded for invalid source %s", source)
		}
		after, err := os.ReadDir(backupDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Fatalf("failed backup pruned or left artifacts: %v", after)
		}
		for name, expected := range before {
			actual, err := os.ReadFile(filepath.Join(backupDir, name))
			if err != nil || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("failed backup changed %s: %v", name, err)
			}
		}
	}
}

// writeBackupFixture creates a real SQLite snapshot for retention and failure tests.
func writeBackupFixture(t *testing.T, root, stamp string) string {
	t.Helper()
	path := filepath.Join(root, "mailbox-"+stamp+".sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE marker (stamp TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO marker VALUES (?)", stamp); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixtureChecksum(t, path)
	return path
}

// writeFixtureChecksum signs fixture bytes even when testing corrupt or symlinked files.
func writeFixtureChecksum(t *testing.T, path string) {
	t.Helper()
	digest, err := backupDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("%s  %s\n", digest, filepath.Base(path))
	if err := os.WriteFile(checksumPath(path), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
