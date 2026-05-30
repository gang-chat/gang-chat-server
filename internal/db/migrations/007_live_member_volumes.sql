CREATE TABLE IF NOT EXISTS live_member_volumes (
    room_id TEXT NOT NULL,
    listener_user_id TEXT NOT NULL,
    target_user_id TEXT NOT NULL,
    volume INTEGER NOT NULL DEFAULT 100 CHECK (volume BETWEEN 0 AND 100),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(room_id, listener_user_id, target_user_id),
    CHECK (listener_user_id <> target_user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(listener_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(target_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_live_member_volumes_listener
ON live_member_volumes(listener_user_id, room_id);
