package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupDatabase writes a logical Postgres dump and prunes old snapshots.
//
// It shells out to pg_dump rather than reimplementing a dump: the tool ships
// with the server image, understands every type the schema uses, and produces an
// artifact that psql can restore without a matching application version.
func backupDatabase(ctx context.Context, dsn, imageDir string, now time.Time) (string, error) {
	dir := filepath.Join(imageDir, "backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	name := fmt.Sprintf("ark-vault-%s.dump", now.Format("20060102-150405"))
	full := filepath.Join(dir, name)

	tmp := full + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("create backup file: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--dbname="+dsn)
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := f.Close()
	if runErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("pg_dump: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close backup file: %w", closeErr)
	}
	// Rename only after a complete dump, so a partial file can never be mistaken
	// for a restorable snapshot.
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("finalise backup: %w", err)
	}

	if err := pruneBackups(dir, 7); err != nil {
		return full, err
	}
	return full, nil
}

// pruneBackups keeps the newest keep snapshots and deletes the rest.
func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}
	var dumps []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".dump") {
			continue
		}
		dumps = append(dumps, e.Name())
	}
	if len(dumps) <= keep {
		return nil
	}
	sort.Strings(dumps) // timestamped names sort chronologically
	for _, name := range dumps[:len(dumps)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove old backup %s: %w", name, err)
		}
	}
	return nil
}
