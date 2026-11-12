package main

import (
	"compress/gzip"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// backupLoop dumps the database with pg_dump on start and then every 24 hours.
// Files go to BACKUP_DIR (default "backups") as db-<date>.sql.gz.
// Files older than BACKUP_KEEP_DAYS (default 14) are deleted. BACKUP_KEEP_DAYS=0 turns backups off.
func backupLoop(databaseURL string) {
	dir := env("BACKUP_DIR", "backups")
	keep := int(envInt("BACKUP_KEEP_DAYS", 14))
	if keep <= 0 {
		return
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		log.Printf("backup: pg_dump not found, backups are off (install the postgresql client)")
		return
	}
	for {
		if err := backup(databaseURL, dir); err != nil {
			log.Printf("backup FAILED: %v", err)
		}
		cleanBackups(dir, keep)
		time.Sleep(24 * time.Hour)
	}
}

func backup(databaseURL, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := filepath.Join(dir, "db-"+time.Now().Format("2006-01-02_1504")+".sql.gz")
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op after the rename

	zw := gzip.NewWriter(f)
	cmd := exec.Command("pg_dump", "--no-owner", "--dbname="+databaseURL)
	cmd.Stdout = zw
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	zw.Close()
	f.Close()
	if runErr != nil {
		return fmt.Errorf("pg_dump: %v: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	log.Printf("backup ok: %s", name)
	return nil
}

func cleanBackups(dir string, keepDays int) {
	files, _ := filepath.Glob(filepath.Join(dir, "db-*.sql.gz"))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil && time.Since(info.ModTime()) > time.Duration(keepDays)*24*time.Hour {
			os.Remove(f)
		}
	}
}
