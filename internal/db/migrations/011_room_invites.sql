CREATE TABLE IF NOT EXISTS room_invites (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    inviter_user_id TEXT NOT NULL,
    target_user_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(room_id, target_user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(inviter_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(target_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_room_invites_target
ON room_invites(target_user_id, status, created_at);
