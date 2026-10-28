// Package db opens the server's SQLite database.
//
// The database is one file next to the server (database_path in config.json).
// It runs in WAL mode, so reads never wait for writes; writes take turns.
// Every transaction starts as BEGIN IMMEDIATE and takes the write lock up
// front, which stands in for MySQL's SELECT ... FOR UPDATE row locks: a
// transaction that reads and then writes can't be overtaken in between.
// Statements wait up to busyTimeout for the lock instead of failing.
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const busyTimeoutMillis = 10000

//go:embed schema.sql
var schemaV1 string

// migrations[i] moves the database from version i to i+1 (PRAGMA
// user_version). Append new ones; never edit one that has shipped.
var migrations = []string{
	schemaV1,
}

// Connect opens (creating if needed) the database at path and brings its schema
// up to date. It exits the process on failure, as the server can't run without
// it.
func Connect(path string) *sql.DB {
	pool, err := Open(path)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	return pool
}

// Open is Connect that returns its error.
func Open(path string) (*sql.DB, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("database_path is empty")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	pool, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, err
	}
	// Each connection holds its own page cache; a handful is plenty for one
	// writer at a time and concurrent WAL readers.
	pool.SetMaxOpenConns(16)
	pool.SetMaxIdleConns(16)
	pool.SetConnMaxLifetime(0)
	if err := pool.Ping(); err != nil {
		pool.Close()
		return nil, err
	}
	if err := Migrate(pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// DSN is the driver connection string for the database file at path, with the
// settings every connection needs.
func DSN(path string) string {
	query := url.Values{}
	for _, pragma := range []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis),
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(1)",
	} {
		query.Add("_pragma", pragma)
	}
	query.Set("_txlock", "immediate")
	return "file:" + path + "?" + query.Encode()
}

// Migrate applies the migrations the database hasn't run yet, each in its own
// transaction.
func Migrate(pool *sql.DB) error {
	var version int
	if err := pool.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this server (%d)", version, len(migrations))
	}
	for next := version; next < len(migrations); next++ {
		tx, err := pool.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", next+1, err)
		}
		if _, err := tx.Exec(migrations[next]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", next+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, next+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema version %d: %w", next+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", next+1, err)
		}
	}
	return nil
}

// IsUniqueViolation reports whether err is a primary key or unique index
// conflict (MySQL's error 1062).
func IsUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code()
	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// IsForeignKeyViolation reports whether err is a foreign key failure (MySQL's
// errors 1451 and 1452).
func IsForeignKeyViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}
