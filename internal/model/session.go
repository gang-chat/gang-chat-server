package model

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"time"
)

type UserSession struct {
	ID               string
	UserID           string
	RefreshTokenHash string
	UserAgent        *string
	IPAddress        *string
	ExpiresAt        int64
	RevokedAt        *int64
	CreatedAt        int64
	LastUsedAt       int64
}

func CreateSession(db *sql.DB, id, userID, refreshTokenHash string, userAgent, ip *string, expiresAt int64) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO user_sessions (id, user_id, refresh_token_hash, user_agent, ip_address, expires_at, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, refreshTokenHash, userAgent, ip, expiresAt, now, now,
	)
	return err
}

func GetSessionByRefreshToken(db *sql.DB, token string) (*UserSession, error) {
	hash := hashToken(token)
	s := &UserSession{}
	err := db.QueryRow(
		`SELECT id, user_id, refresh_token_hash, user_agent, ip_address, expires_at, revoked_at, created_at, last_used_at
		 FROM user_sessions WHERE refresh_token_hash = ?`, hash,
	).Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.UserAgent, &s.IPAddress, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt, &s.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func GetSessionByID(db *sql.DB, id string) (*UserSession, error) {
	s := &UserSession{}
	err := db.QueryRow(
		`SELECT id, user_id, refresh_token_hash, user_agent, ip_address, expires_at, revoked_at, created_at, last_used_at
		 FROM user_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.UserAgent, &s.IPAddress, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt, &s.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func RevokeSession(db *sql.DB, id string) error {
	now := time.Now().Unix()
	_, err := db.Exec(`UPDATE user_sessions SET revoked_at = ? WHERE id = ?`, now, id)
	return err
}

func RevokeSessionByRefreshToken(db *sql.DB, token string) error {
	hash := hashToken(token)
	now := time.Now().Unix()
	_, err := db.Exec(`UPDATE user_sessions SET revoked_at = ? WHERE refresh_token_hash = ?`, now, hash)
	return err
}

func RotateRefreshToken(db *sql.DB, sessionID, newToken string) error {
	newHash := hashToken(newToken)
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE user_sessions SET refresh_token_hash = ?, last_used_at = ? WHERE id = ?`,
		newHash, now, sessionID,
	)
	return err
}

func ListActiveSessions(db *sql.DB, userID string) ([]UserSession, error) {
	now := time.Now().Unix()
	rows, err := db.Query(
		`SELECT id, user_id, refresh_token_hash, user_agent, ip_address, expires_at, revoked_at, created_at, last_used_at
		 FROM user_sessions
		 WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?
		 ORDER BY created_at DESC`, userID, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []UserSession
	for rows.Next() {
		s := UserSession{}
		if err := rows.Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.UserAgent, &s.IPAddress, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt, &s.LastUsedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func RevokeAllOtherSessions(db *sql.DB, userID, keepSessionID string) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND id != ? AND revoked_at IS NULL`,
		now, userID, keepSessionID,
	)
	return err
}

// HashTokenForCreate exports the token hashing for use by the auth package.
func HashTokenForCreate(token string) string {
	return hashToken(token)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
