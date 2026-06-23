CREATE TABLE IF NOT EXISTS room_blacklist (
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    blocked_by_user_id TEXT,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(blocked_by_user_id) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_room_blacklist_user
ON room_blacklist(user_id, room_id);
