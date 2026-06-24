CREATE TABLE IF NOT EXISTS room_notifications (
    id TEXT PRIMARY KEY NOT NULL,
    recipient_user_id TEXT NOT NULL,
    room_id TEXT NOT NULL,
    actor_user_id TEXT,
    type TEXT NOT NULL,
    from_role TEXT,
    to_role TEXT,
    created_at INTEGER NOT NULL,
    room_rid TEXT NOT NULL DEFAULT '',
    room_name TEXT NOT NULL DEFAULT '',
    room_avatar_url TEXT,
    room_default_avatar_key TEXT NOT NULL DEFAULT 'room-1',
    room_visibility TEXT NOT NULL DEFAULT 'private',
    room_join_policy TEXT NOT NULL DEFAULT 'closed',
    room_description TEXT NOT NULL DEFAULT '',
    room_created_by_user_id TEXT,
    actor_uid TEXT,
    actor_username TEXT,
    actor_display_name TEXT,
    actor_avatar_url TEXT,
    actor_default_avatar_key TEXT NOT NULL DEFAULT 'blue-3',
    actor_room_display_name TEXT,
    actor_room_role TEXT,
    FOREIGN KEY(recipient_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_room_notifications_recipient_created
ON room_notifications(recipient_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_room_notifications_room
ON room_notifications(room_id, created_at DESC);
