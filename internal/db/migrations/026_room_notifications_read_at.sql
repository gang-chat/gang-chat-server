ALTER TABLE room_notifications ADD COLUMN read_at INTEGER;

CREATE INDEX IF NOT EXISTS idx_room_notifications_recipient_read
ON room_notifications(recipient_user_id, read_at, created_at DESC);
