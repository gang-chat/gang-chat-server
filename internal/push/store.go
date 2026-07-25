package push

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type DeviceRegistration struct {
	Provider       string
	InstallationID string
	Token          string
	Platform       string
	Enabled        bool
}

type Store struct {
	DB *sql.DB
}

func (s *Store) EnsureSchema() error {
	if s == nil || s.DB == nil {
		return errors.New("push store database is required")
	}
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS push_devices (
		provider VARCHAR(32) NOT NULL,
		installation_id VARCHAR(128) NOT NULL,
		user_id VARCHAR(128) NOT NULL,
		session_id VARCHAR(128) NOT NULL,
		token VARCHAR(1024) NOT NULL,
		platform VARCHAR(32) NOT NULL DEFAULT 'android',
		notifications_enabled TINYINT(1) NOT NULL DEFAULT 1,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		PRIMARY KEY (provider, installation_id),
		KEY idx_push_devices_user (user_id, notifications_enabled),
		KEY idx_push_devices_session (session_id),
		CONSTRAINT fk_push_devices_user FOREIGN KEY (user_id)
			REFERENCES users(id) ON DELETE CASCADE,
		CONSTRAINT fk_push_devices_session FOREIGN KEY (session_id)
			REFERENCES user_sessions(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`)
	return err
}

func (s *Store) Upsert(userID, sessionID string, registration DeviceRegistration) error {
	now := time.Now().UnixMilli()
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// FCM can restore the same token to a reinstalled app. Keep exactly one
	// owner for a provider token so a message is never delivered twice.
	if _, err := tx.Exec(
		`DELETE FROM push_devices
		 WHERE provider = ? AND token = ? AND installation_id <> ?`,
		registration.Provider,
		registration.Token,
		registration.InstallationID,
	); err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO push_devices (
			provider, installation_id, user_id, session_id, token, platform,
			notifications_enabled, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			user_id = VALUES(user_id),
			session_id = VALUES(session_id),
			token = VALUES(token),
			platform = VALUES(platform),
			notifications_enabled = VALUES(notifications_enabled),
			updated_at = VALUES(updated_at)`,
		registration.Provider,
		registration.InstallationID,
		userID,
		sessionID,
		registration.Token,
		registration.Platform,
		registration.Enabled,
		now,
		now,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetEnabled(userID, provider, installationID string, enabled bool) (bool, error) {
	result, err := s.DB.Exec(
		`UPDATE push_devices
		 SET notifications_enabled = ?, updated_at = ?
		 WHERE user_id = ? AND provider = ? AND installation_id = ?`,
		enabled,
		time.Now().UnixMilli(),
		userID,
		provider,
		installationID,
	)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected > 0, nil
}

func (s *Store) Delete(userID, provider, installationID string) error {
	_, err := s.DB.Exec(
		`DELETE FROM push_devices
		 WHERE user_id = ? AND provider = ? AND installation_id = ?`,
		userID,
		strings.ToLower(strings.TrimSpace(provider)),
		strings.TrimSpace(installationID),
	)
	return err
}

func (s *Store) DeleteToken(provider, token string) {
	if s == nil || s.DB == nil || token == "" {
		return
	}
	_, _ = s.DB.Exec(
		`DELETE FROM push_devices WHERE provider = ? AND token = ?`,
		provider,
		token,
	)
}
