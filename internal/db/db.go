package db

import (
	"database/sql"
	"embed"
	"log"
	"os"
	"path/filepath"
	"sort"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations
var migrationFS embed.FS

func Connect(dsn string) *sql.DB {
	dir := filepath.Dir(dsn)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create db dir: %v", err)
		}
	}

	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_foreign_keys=1")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	if err := migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	return db
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY NOT NULL, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		data, err := migrationFS.ReadFile(filepath.ToSlash(filepath.Join("migrations", name)))
		if err != nil {
			return err
		}

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(data)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, unixepoch())`, name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
