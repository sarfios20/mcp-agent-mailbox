package admin

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type BackupResult struct {
	Snapshot string   `json:"snapshot"`
	Kept     int      `json:"kept"`
	Deleted  []string `json:"deleted"`
}

// Backup switches to the data owner and holds one lock for snapshot creation and pruning.
func Backup(dataDir string) (BackupResult, error) {
	if err := BecomeDataOwner(dataDir); err != nil {
		return BackupResult{}, err
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, BackupLockName), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return BackupResult{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return BackupResult{}, errors.New("another mailbox backup is already running")
		}
		return BackupResult{}, fmt.Errorf("lock backup: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return createBackup(filepath.Join(dataDir, "mailbox.db"), BackupDir, time.Now)
}

// createBackup takes a source path, destination directory and clock; pruning starts only
// after the published VACUUM INTO snapshot passes integrity and checksum verification.
func createBackup(database, root string, clock func() time.Time) (BackupResult, error) {
	info, err := os.Stat(database)
	if err != nil {
		return BackupResult{}, fmt.Errorf("mailbox database %s: %w", database, err)
	}
	if !info.Mode().IsRegular() {
		return BackupResult{}, fmt.Errorf("mailbox database is not a regular file: %s", database)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return BackupResult{}, err
	}
	destination := filepath.Join(root, "mailbox-"+clock().UTC().Format("20060102T150405Z")+".sqlite")
	for _, path := range []string{destination, checksumPath(destination)} {
		if _, err := os.Lstat(path); err == nil {
			return BackupResult{}, fmt.Errorf("backup already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return BackupResult{}, err
		}
	}
	temporary, err := os.CreateTemp(root, "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return BackupResult{}, err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	defer os.Remove(tempPath + "-wal")
	defer os.Remove(tempPath + "-shm")
	if err := temporary.Close(); err != nil {
		return BackupResult{}, err
	}
	source, err := openBackupDatabase(database, false)
	if err != nil {
		return BackupResult{}, err
	}
	_, vacuumErr := source.Exec("VACUUM INTO ?", tempPath)
	closeErr := source.Close()
	if err := errors.Join(vacuumErr, closeErr); err != nil {
		return BackupResult{}, fmt.Errorf("snapshot database: %w", err)
	}
	if err := verifyBackupDatabase(tempPath); err != nil {
		return BackupResult{}, err
	}
	digest, err := backupDigest(tempPath)
	if err != nil {
		return BackupResult{}, err
	}
	sidecar, err := os.CreateTemp(root, "."+filepath.Base(destination)+".sha256.tmp-*")
	if err != nil {
		return BackupResult{}, err
	}
	defer os.Remove(sidecar.Name())
	_, writeErr := fmt.Fprintf(sidecar, "%s  %s\n", digest, filepath.Base(destination))
	closeErr = sidecar.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return BackupResult{}, err
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return BackupResult{}, err
	}
	if err := os.Rename(sidecar.Name(), checksumPath(destination)); err != nil {
		return BackupResult{}, err
	}
	if err := verifySnapshot(destination); err != nil {
		return BackupResult{}, err
	}
	kept, deleted, err := pruneBackups(root)
	if err != nil {
		return BackupResult{}, err
	}
	return BackupResult{Snapshot: destination, Kept: kept, Deleted: deleted}, nil
}

// openBackupDatabase opens an existing database read-only; immutable copies need no WAL files.
func openBackupDatabase(path string, immutable bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := &url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"mode": {"ro"}}
	if immutable {
		query.Set("immutable", "1")
	}
	uri.RawQuery = query.Encode()
	return sql.Open("sqlite", uri.String())
}

// verifyBackupDatabase requires every integrity_check result to be ok.
func verifyBackupDatabase(path string) error {
	db, err := openBackupDatabase(path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("SQLite integrity check failed for %s: %s", path, result)
		}
		seen = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !seen {
		return fmt.Errorf("SQLite integrity check returned no result for %s", path)
	}
	return nil
}

// checksumPath returns the sha256sum sidecar beside a snapshot.
func checksumPath(snapshot string) string {
	return strings.TrimSuffix(snapshot, ".sqlite") + ".sha256"
}

// backupDigest hashes a file without loading the database into memory.
func backupDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// verifySnapshot excludes symlinks and requires a matching sidecar and a valid SQLite database.
func verifySnapshot(path string) error {
	for _, candidate := range []string{path, checksumPath(path)} {
		info, err := os.Lstat(candidate)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular backup file: %s", candidate)
		}
	}
	data, err := os.ReadFile(checksumPath(path))
	if err != nil {
		return err
	}
	fields := strings.Fields(string(data))
	digest, err := backupDigest(path)
	if err != nil {
		return err
	}
	if len(fields) != 2 || fields[0] != digest || fields[1] != filepath.Base(path) {
		return fmt.Errorf("backup checksum mismatch: %s", path)
	}
	return verifyBackupDatabase(path)
}

// snapshotTime accepts only canonical UTC snapshot filenames with valid dates.
func snapshotTime(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, "mailbox-") || !strings.HasSuffix(name, ".sqlite") {
		return time.Time{}, false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "mailbox-"), ".sqlite")
	date, err := time.Parse("20060102T150405Z", stamp)
	return date, err == nil && date.Format("20060102T150405Z") == stamp
}

// pruneBackups retains the union of newest snapshots per distinct calendar bucket.
// Malformed, unchecksummed, corrupt or symlinked files are never deletion candidates.
func pruneBackups(root string) (int, []string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, nil, err
	}
	type snapshot struct {
		name string
		date time.Time
	}
	var snapshots []snapshot
	for _, entry := range entries {
		date, valid := snapshotTime(entry.Name())
		if valid && verifySnapshot(filepath.Join(root, entry.Name())) == nil {
			snapshots = append(snapshots, snapshot{entry.Name(), date})
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].date.After(snapshots[j].date) })
	retained := make(map[string]bool)
	policies := []struct {
		limit  int
		bucket func(time.Time) string
	}{
		{BackupDailyCount, func(t time.Time) string { return t.Format("2006-01-02") }},
		{BackupWeeklyCount, func(t time.Time) string {
			year, week := t.ISOWeek()
			return fmt.Sprintf("%d-%02d", year, week)
		}},
		{BackupMonthlyCount, func(t time.Time) string { return t.Format("2006-01") }},
		{BackupYearlyCount, func(t time.Time) string { return t.Format("2006") }},
	}
	for _, policy := range policies {
		buckets := make(map[string]bool)
		for _, snapshot := range snapshots {
			bucket := policy.bucket(snapshot.date)
			if buckets[bucket] {
				continue
			}
			if len(buckets) >= policy.limit {
				break
			}
			buckets[bucket] = true
			retained[snapshot.name] = true
		}
	}
	deleted := []string{}
	for _, snapshot := range snapshots {
		if retained[snapshot.name] {
			continue
		}
		path := filepath.Join(root, snapshot.name)
		if err := os.Remove(path); err != nil {
			return len(retained), deleted, err
		}
		if err := os.Remove(checksumPath(path)); err != nil {
			return len(retained), deleted, err
		}
		deleted = append(deleted, snapshot.name)
	}
	return len(retained), deleted, nil
}
