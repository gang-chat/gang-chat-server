package idgen

import (
	"database/sql"
	"log"
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
func NextUserUID(db *sql.DB) string {
	return nextSeq(db, "user_uid", UserUIDStart)
}

// NextRoomRID atomically allocates the next human-facing room rid.
func NextRoomRID(db *sql.DB) string {
	return nextSeq(db, "room_rid", RoomRIDStart)
}

// nextSeq atomically allocates and returns the next value for the named
// sequence. The row is locked with SELECT ... FOR UPDATE, so concurrent callers
// cannot receive the same id. It also repairs older deployments where the
// sequence table or row is missing by seeding from already-visible numeric ids.
func nextSeq(db *sql.DB, name string, start int64) string {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("idgen: begin sequence transaction %s: %v", name, err)
		return strconv.FormatInt(start, 10)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`CREATE TABLE IF NOT EXISTS id_sequences (
			name VARCHAR(64) PRIMARY KEY NOT NULL,
			next_value BIGINT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	); err != nil {
		log.Printf("idgen: ensure sequence table %s: %v", name, err)
		return strconv.FormatInt(start, 10)
	}

	seed := sequenceSeedValue(tx, name, start)
	if _, err := tx.Exec(
		`INSERT INTO id_sequences (name, next_value)
		 VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE next_value = GREATEST(next_value, VALUES(next_value))`,
		name, seed,
	); err != nil {
		log.Printf("idgen: ensure sequence row %s seed=%d: %v", name, seed, err)
		return strconv.FormatInt(start, 10)
	}

	allocated := start
	if err := tx.QueryRow(`SELECT next_value FROM id_sequences WHERE name = ? FOR UPDATE`, name).Scan(&allocated); err != nil {
		log.Printf("idgen: lock sequence %s: %v", name, err)
		return strconv.FormatInt(start, 10)
	}
	if allocated < seed {
		allocated = seed
	}
	if _, err := tx.Exec(`UPDATE id_sequences SET next_value = ? WHERE name = ?`, allocated+1, name); err != nil {
		log.Printf("idgen: advance sequence %s from %d: %v", name, allocated, err)
		return strconv.FormatInt(start, 10)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("idgen: commit sequence %s: %v", name, err)
		return strconv.FormatInt(start, 10)
	}
	return strconv.FormatInt(allocated, 10)
}

func sequenceSeedValue(tx *sql.Tx, name string, start int64) int64 {
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
		return start
	}

	seed := start
	if err := tx.QueryRow(query, start).Scan(&seed); err != nil {
		return start
	}
	if seed < start {
		return start
	}
	return seed
}
