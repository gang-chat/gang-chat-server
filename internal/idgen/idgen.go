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

func NextUserUID(db *sql.DB) string {
	query := "SELECT COALESCE(MAX(CAST(uid AS INTEGER)), ?) + 1 FROM users WHERE uid <> ?"
	var n int64
	if err := db.QueryRow(query, UserUIDStart-1, ReservedSuperUID).Scan(&n); err != nil || n < UserUIDStart {
		n = UserUIDStart
	}
	return strconv.FormatInt(n, 10)
}

func NextRoomRID(db *sql.DB) string {
	return nextNumeric(db, "rooms", "rid", RoomRIDStart)
}

func nextNumeric(db *sql.DB, table, column string, start int64) string {
	query := "SELECT COALESCE(MAX(CAST(" + column + " AS INTEGER)), ?) + 1 FROM " + table
	var n int64
	if err := db.QueryRow(query, start-1).Scan(&n); err != nil || n < start {
		n = start
	}
	return strconv.FormatInt(n, 10)
}
