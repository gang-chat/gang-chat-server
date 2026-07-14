package idgen

import (
	"database/sql"
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

const (
	UserUIDStart       = int64(10000000)
	RoomRIDStart       = int64(20000000)
	ReservedSuperUID   = "66666666"
	ReservedSuperEmail = "gang-chat@outlook.com"
	ReservedSuperName  = "GANG"
)

func New(prefix string) string {
	return prefix + "_" + uuid.NewString()
}

// NextUserUID atomically allocates the next human-facing user uid.
func NextUserUID(db *sql.DB) (string, error) {
	return nextSeq(db, "user_uid", UserUIDStart)
}

// NextRoomRID atomically allocates the next human-facing room rid.
func NextRoomRID(db *sql.DB) (string, error) {
	return nextSeq(db, "room_rid", RoomRIDStart)
}

// nextSeq atomically allocates and returns the next value for the named
// sequence. The row is locked with SELECT ... FOR UPDATE, so concurrent callers
// cannot receive the same id. It also repairs older deployments where the
// sequence table or row is missing by seeding from already-visible numeric ids.
func nextSeq(db *sql.DB, name string, start int64) (string, error) {
	if db == nil {
		return "", fmt.Errorf("allocate %s: database is nil", name)
	}
	// MySQL implicitly commits around DDL. Creating the compatibility table
	// inside the allocation transaction would therefore release the row lock
	// used below and allow concurrent callers to receive the same value.
	if _, err := db.Exec(
		`CREATE TABLE IF NOT EXISTS id_sequences (
			name VARCHAR(64) PRIMARY KEY NOT NULL,
			next_value BIGINT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	); err != nil {
		return "", fmt.Errorf("ensure sequence table %s: %w", name, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin sequence transaction %s: %w", name, err)
	}
	defer tx.Rollback()

	seed, err := sequenceSeedValue(tx, name, start)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO id_sequences (name, next_value)
		 VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE next_value = GREATEST(next_value, VALUES(next_value))`,
		name, seed,
	); err != nil {
		return "", fmt.Errorf("ensure sequence row %s seed=%d: %w", name, seed, err)
	}

	allocated := start
	if err := tx.QueryRow(`SELECT next_value FROM id_sequences WHERE name = ? FOR UPDATE`, name).Scan(&allocated); err != nil {
		return "", fmt.Errorf("lock sequence %s: %w", name, err)
	}
	if allocated < seed {
		allocated = seed
	}
	if _, err := tx.Exec(`UPDATE id_sequences SET next_value = ? WHERE name = ?`, allocated+1, name); err != nil {
		return "", fmt.Errorf("advance sequence %s from %d: %w", name, allocated, err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit sequence %s: %w", name, err)
	}
	return strconv.FormatInt(allocated, 10), nil
}

func sequenceSeedValue(tx *sql.Tx, name string, start int64) (int64, error) {
	query := ""
	switch name {
	case "user_uid":
		query = `SELECT COALESCE(MAX(CAST(uid AS UNSIGNED)) + 1, ?)
			FROM users
			WHERE uid REGEXP '^[0-9]+$'`
	case "room_rid":
		query = `SELECT COALESCE(MAX(CAST(rid AS UNSIGNED)) + 1, ?)
			FROM rooms
			WHERE rid REGEXP '^[0-9]+$'`
	default:
		return start, nil
	}

	seed := start
	if err := tx.QueryRow(query, start).Scan(&seed); err != nil {
		return 0, fmt.Errorf("read sequence seed %s: %w", name, err)
	}
	if seed < start {
		return start, nil
	}
	return seed, nil
}
