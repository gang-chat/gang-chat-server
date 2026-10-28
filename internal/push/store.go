package push

import (
	"database/sql"
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
		 ON CONFLICT DO UPDATE SET
			user_id = excluded.user_id,
			session_id = excluded.session_id,
			token = excluded.token,
			platform = excluded.platform,
			notifications_enabled = excluded.notifications_enabled,
			updated_at = excluded.updated_at`,
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
