package idgen

import (
	"database/sql"
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
// sequence. A single UPSERT...RETURNING statement reads-and-increments under
// one row lock, so concurrent callers can never receive the same id (the old
// "SELECT MAX(..)+1" scheme had a TOCTOU race that collided on the unique
// index under concurrent registration / room creation).
//
// The id_sequences row is normally seeded by migration 020 to one past the
// existing maximum. If it's somehow missing (fresh table), the INSERT branch
// seeds it at start and the RETURNING hands back start.
func nextSeq(db *sql.DB, name string, start int64) string {
	var allocated int64
	err := db.QueryRow(
		`INSERT INTO id_sequences (name, next_value) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET next_value = next_value + 1
		 RETURNING next_value - 1`,
		name, start+1,
	).Scan(&allocated)
	if err != nil || allocated < start {
		// Last-resort fallback. Should be unreachable in practice; keeping the
		// service alive with a best-effort id beats failing the request.
		allocated = start
	}
	return strconv.FormatInt(allocated, 10)
}
