CREATE TABLE IF NOT EXISTS music_sessions (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'stopped',
    mode TEXT NOT NULL DEFAULT 'sequential',
    playlist_id TEXT,
    playlist_scope TEXT,
    current_queue_id TEXT,
    follow_user_id TEXT,
    position_ms INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER,
    updated_at INTEGER NOT NULL,
    UNIQUE(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(current_queue_id) REFERENCES music_queue(id) ON DELETE SET NULL,
    FOREIGN KEY(follow_user_id) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_music_sessions_room_state
ON music_sessions(room_id, state, updated_at);

CREATE TABLE IF NOT EXISTS music_invites (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    inviter_user_id TEXT NOT NULL,
    target_user_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(session_id) REFERENCES music_sessions(id) ON DELETE CASCADE,
    FOREIGN KEY(inviter_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(target_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_music_invites_target
ON music_invites(room_id, target_user_id, status, created_at);
