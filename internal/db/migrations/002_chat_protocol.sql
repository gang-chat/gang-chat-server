CREATE TABLE IF NOT EXISTS rooms (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    avatar_url TEXT,
    default_avatar_key TEXT NOT NULL,
    created_by_user_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(created_by_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS room_memberships (
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    joined_at INTEGER NOT NULL,
    PRIMARY KEY(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_room_memberships_user_id ON room_memberships(user_id);

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    sender_user_id TEXT NOT NULL,
    client_message_id TEXT NOT NULL,
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(room_id, sender_user_id, client_message_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(sender_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_room_created_at ON messages(room_id, created_at);

CREATE TABLE IF NOT EXISTS room_reads (
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    last_read_message_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(last_read_message_id) REFERENCES messages(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS live_participants (
    live_session_id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    client_live_session_id TEXT NOT NULL,
    joined_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    mic_muted INTEGER NOT NULL DEFAULT 1,
    camera_on INTEGER NOT NULL DEFAULT 0,
    screen_sharing INTEGER NOT NULL DEFAULT 0,
    connection_state TEXT NOT NULL DEFAULT 'joining',
    UNIQUE(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_live_participants_room_id ON live_participants(room_id);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id TEXT NOT NULL,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response_status INTEGER NOT NULL,
    response_body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, method, path, idempotency_key),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created_at ON idempotency_keys(created_at);
