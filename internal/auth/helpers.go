package auth

import (
	"database/sql"
	"strconv"
)

func nullablePtrString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullableOr(value sql.NullString, fallback string) string {
	if !value.Valid || value.String == "" {
		return fallback
	}
	return value.String
}

func strconvItoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func parseSmallPositiveInt(raw string, max int) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if n <= 0 || n > max {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}
