package model

import (
	"database/sql"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

type User struct {
	ID                 string
	UID                sql.NullString
	Username           string
	UsernameNormalized string
	Email              string
	EmailNormalized    string
	PasswordHash       *string
	Status             string
	DisplayName        sql.NullString
	Bio                sql.NullString
	AvatarURL          sql.NullString
	DefaultAvatarKey   sql.NullString
	EmailVerified      bool
	IsSuperuser        bool
	UsernameUpdatedAt  sql.NullInt64
	DeletedAt          sql.NullInt64
	CreatedAt          int64
	UpdatedAt          int64
}

func CreateUser(db *sql.DB, id, username, usernameNorm, email, emailNorm, passwordHash string) (*User, error) {
	now := time.Now().Unix()
	uid := idgen.NextUserUID(db)
	_, err := db.Exec(
		`INSERT INTO users (
		   id, uid, username, username_normalized, email, email_normalized, password_hash,
		   status, display_name, default_avatar_key, email_verified, username_updated_at,
		   created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, 'blue-3', 1, ?, ?, ?)`,
		id, uid, username, usernameNorm, email, emailNorm, passwordHash, username, now, now, now,
	)
	if err != nil {
		return nil, err
	}
	return GetUserByID(db, id)
}

func GetUserByID(db *sql.DB, id string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id, uid, username, username_normalized, email, email_normalized, password_hash,
		        status, display_name, bio, avatar_url, default_avatar_key, email_verified,
		        is_superuser, username_updated_at, deleted_at, created_at, updated_at
		 FROM users WHERE id = ?`, id,
	).Scan(
		&u.ID, &u.UID, &u.Username, &u.UsernameNormalized, &u.Email, &u.EmailNormalized, &u.PasswordHash,
		&u.Status, &u.DisplayName, &u.Bio, &u.AvatarURL, &u.DefaultAvatarKey, &u.EmailVerified,
		&u.IsSuperuser, &u.UsernameUpdatedAt, &u.DeletedAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if !u.UID.Valid || u.UID.String == "" {
		if err := EnsureUserPublicFields(db, id); err != nil {
			return nil, err
		}
		return GetUserByID(db, id)
	}
	return u, nil
}

func GetUserByUsernameOrEmail(db *sql.DB, login string) (*User, error) {
	norm := normalize(login)
	u := &User{}
	err := db.QueryRow(
		`SELECT id, uid, username, username_normalized, email, email_normalized, password_hash,
		        status, display_name, bio, avatar_url, default_avatar_key, email_verified,
		        is_superuser, username_updated_at, deleted_at, created_at, updated_at
		 FROM users WHERE username_normalized = ? OR email_normalized = ?`,
		norm, norm,
	).Scan(
		&u.ID, &u.UID, &u.Username, &u.UsernameNormalized, &u.Email, &u.EmailNormalized, &u.PasswordHash,
		&u.Status, &u.DisplayName, &u.Bio, &u.AvatarURL, &u.DefaultAvatarKey, &u.EmailVerified,
		&u.IsSuperuser, &u.UsernameUpdatedAt, &u.DeletedAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if !u.UID.Valid || u.UID.String == "" {
		if err := EnsureUserPublicFields(db, u.ID); err != nil {
			return nil, err
		}
		return GetUserByID(db, u.ID)
	}
	return u, nil
}

func UpdatePassword(db *sql.DB, userID, hash string) error {
	_, err := db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, time.Now().Unix(), userID)
	return err
}

func normalize(s string) string {
	// simple lowercase for lookup; same as Rust username_normalized / email_normalized
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		result = append(result, c)
	}
	return string(result)
}

func EnsureUserPublicFields(db *sql.DB, userID string) error {
	u, err := GetUserByIDLoose(db, userID)
	if err != nil {
		return err
	}
	uid := u.UID.String
	if uid == "" {
		uid = idgen.NextUserUID(db)
	}
	displayName := u.DisplayName.String
	if displayName == "" {
		displayName = u.Username
	}
	defaultAvatar := u.DefaultAvatarKey.String
	if defaultAvatar == "" {
		defaultAvatar = "blue-3"
	}
	usernameUpdatedAt := u.UsernameUpdatedAt.Int64
	if usernameUpdatedAt == 0 {
		usernameUpdatedAt = u.CreatedAt
	}
	_, err = db.Exec(
		`UPDATE users
		 SET uid = ?, display_name = ?, default_avatar_key = ?, username_updated_at = ?, updated_at = ?
		 WHERE id = ?`,
		uid, displayName, defaultAvatar, usernameUpdatedAt, time.Now().Unix(), userID,
	)
	return err
}

func GetUserByIDLoose(db *sql.DB, id string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id, uid, username, username_normalized, email, email_normalized, password_hash,
		        status, display_name, bio, avatar_url, default_avatar_key, email_verified,
		        is_superuser, username_updated_at, deleted_at, created_at, updated_at
		 FROM users WHERE id = ?`, id,
	).Scan(
		&u.ID, &u.UID, &u.Username, &u.UsernameNormalized, &u.Email, &u.EmailNormalized, &u.PasswordHash,
		&u.Status, &u.DisplayName, &u.Bio, &u.AvatarURL, &u.DefaultAvatarKey, &u.EmailVerified,
		&u.IsSuperuser, &u.UsernameUpdatedAt, &u.DeletedAt, &u.CreatedAt, &u.UpdatedAt,
	)
	return u, err
}

func Normalize(s string) string {
	return normalize(s)
}

func IsSuperuser(db *sql.DB, userID string) bool {
	var isSuperuser int
	_ = db.QueryRow(`SELECT is_superuser FROM users WHERE id = ? AND status = 'active'`, userID).Scan(&isSuperuser)
	return isSuperuser != 0
}
