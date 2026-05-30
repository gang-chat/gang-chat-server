package auth

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

const superUserPassword = "64n9-Ch47"

func (h *Handler) ensureSuperUser() error {
	var id string
	err := h.DB.QueryRow(
		`SELECT id
		 FROM users
		 WHERE uid = ? OR username_normalized = ? OR email_normalized = ?
		 ORDER BY is_superuser DESC
		 LIMIT 1`,
		idgen.ReservedSuperUID, strings.ToLower(idgen.ReservedSuperName), idgen.ReservedSuperEmail,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		hash, err := HashPassword(superUserPassword)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		_, err = h.DB.Exec(
			`INSERT INTO users (
			   id, uid, username, username_normalized, email, email_normalized, password_hash,
			   status, display_name, bio, default_avatar_key, email_verified, is_superuser,
			   username_updated_at, created_at, updated_at
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, '', 'blue-3', 1, 1, ?, ?, ?)`,
			uuid.NewString(), idgen.ReservedSuperUID, idgen.ReservedSuperName, strings.ToLower(idgen.ReservedSuperName),
			idgen.ReservedSuperEmail, idgen.ReservedSuperEmail, hash, idgen.ReservedSuperName, now, now, now,
		)
		return err
	}
	if err != nil {
		return err
	}

	hash, err := HashPassword(superUserPassword)
	if err != nil {
		return err
	}
	sets := `uid = ?, username = ?, username_normalized = ?, email = ?, email_normalized = ?, password_hash = ?,
	         status = 'active', display_name = ?, default_avatar_key = COALESCE(NULLIF(default_avatar_key, ''), 'blue-3'),
	         email_verified = 1, is_superuser = 1, deleted_at = NULL, updated_at = ?`
	args := []any{
		idgen.ReservedSuperUID, idgen.ReservedSuperName, strings.ToLower(idgen.ReservedSuperName),
		idgen.ReservedSuperEmail, idgen.ReservedSuperEmail, hash, idgen.ReservedSuperName, time.Now().Unix(),
	}
	args = append(args, id)
	_, err = h.DB.Exec(`UPDATE users SET `+sets+` WHERE id = ?`, args...)
	return err
}
