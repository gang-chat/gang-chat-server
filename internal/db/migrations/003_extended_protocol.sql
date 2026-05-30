ALTER TABLE users ADD COLUMN uid TEXT;
ALTER TABLE users ADD COLUMN display_name TEXT;
ALTER TABLE users ADD COLUMN avatar_url TEXT;
ALTER TABLE users ADD COLUMN default_avatar_key TEXT DEFAULT 'blue-3';
ALTER TABLE users ADD COLUMN email_verified INTEGER NOT NULL DEFAULT 1;
ALTER TABLE users ADD COLUMN username_updated_at INTEGER;

UPDATE users
SET uid = printf('%08d', 10000000 + rowid),
    display_name = COALESCE(display_name, username),
    default_avatar_key = COALESCE(default_avatar_key, 'blue-3'),
    username_updated_at = COALESCE(username_updated_at, created_at)
WHERE uid IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_uid ON users(uid);

ALTER TABLE rooms ADD COLUMN rid TEXT;
ALTER TABLE rooms ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public';
ALTER TABLE rooms ADD COLUMN join_policy TEXT NOT NULL DEFAULT 'open';
ALTER TABLE rooms ADD COLUMN ai_voice_announce_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE rooms ADD COLUMN message_recall_policy TEXT NOT NULL DEFAULT 'time_limited';
ALTER TABLE rooms ADD COLUMN message_recall_window_seconds INTEGER DEFAULT 120;
ALTER TABLE rooms ADD COLUMN avatar_asset_id TEXT;

UPDATE rooms
SET rid = printf('%08d', 20000000 + rowid),
    visibility = COALESCE(visibility, 'public'),
    join_policy = COALESCE(join_policy, 'open'),
    message_recall_policy = COALESCE(message_recall_policy, 'time_limited')
WHERE rid IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_rooms_rid ON rooms(rid);

ALTER TABLE room_memberships ADD COLUMN role TEXT NOT NULL DEFAULT 'member';
ALTER TABLE room_memberships ADD COLUMN remark_name TEXT;
ALTER TABLE room_memberships ADD COLUMN room_display_name TEXT;
ALTER TABLE room_memberships ADD COLUMN room_avatar_url TEXT;
ALTER TABLE room_memberships ADD COLUMN room_default_avatar_key TEXT;
ALTER TABLE room_memberships ADD COLUMN notification_level TEXT NOT NULL DEFAULT 'all';
ALTER TABLE room_memberships ADD COLUMN text_muted_until INTEGER;

UPDATE room_memberships
SET role = 'owner'
WHERE EXISTS (
    SELECT 1
    FROM rooms
    WHERE rooms.id = room_memberships.room_id
      AND rooms.created_by_user_id = room_memberships.user_id
);

ALTER TABLE messages ADD COLUMN type TEXT NOT NULL DEFAULT 'text';
ALTER TABLE messages ADD COLUMN mentions_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE messages ADD COLUMN attachments_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE messages ADD COLUMN is_recalled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN recalled_at INTEGER;
ALTER TABLE messages ADD COLUMN recalled_by_user_id TEXT;
ALTER TABLE messages ADD COLUMN is_force_deleted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN force_deleted_at INTEGER;
ALTER TABLE messages ADD COLUMN force_deleted_by_user_id TEXT;

CREATE TABLE IF NOT EXISTS assets (
    id TEXT PRIMARY KEY NOT NULL,
    owner_user_id TEXT NOT NULL,
    purpose TEXT NOT NULL,
    filename TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    url TEXT NOT NULL,
    thumbnail_url TEXT,
    width INTEGER,
    height INTEGER,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(owner_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS sticker_packs (
    id TEXT PRIMARY KEY NOT NULL,
    owner_user_id TEXT,
    room_id TEXT,
    scope TEXT NOT NULL,
    name TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 10,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(owner_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS stickers (
    id TEXT PRIMARY KEY NOT NULL,
    pack_id TEXT NOT NULL,
    asset_id TEXT NOT NULL,
    name TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 10,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(pack_id) REFERENCES sticker_packs(id) ON DELETE CASCADE,
    FOREIGN KEY(asset_id) REFERENCES assets(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS playlists (
    id TEXT PRIMARY KEY NOT NULL,
    owner_user_id TEXT,
    room_id TEXT,
    scope TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(owner_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS playlist_tracks (
    id TEXT PRIMARY KEY NOT NULL,
    playlist_id TEXT NOT NULL,
    title TEXT NOT NULL,
    artist TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual_url',
    source_url TEXT NOT NULL,
    duration_ms INTEGER,
    sort_order INTEGER NOT NULL DEFAULT 10,
    added_by_user_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(playlist_id) REFERENCES playlists(id) ON DELETE CASCADE,
    FOREIGN KEY(added_by_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS join_requests (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS message_recall_requests (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    requested_by_user_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(room_id, message_id, requested_by_user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(message_id) REFERENCES messages(id) ON DELETE CASCADE,
    FOREIGN KEY(requested_by_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS music_playback (
    room_id TEXT PRIMARY KEY NOT NULL,
    state TEXT NOT NULL DEFAULT 'stopped',
    mode TEXT NOT NULL DEFAULT 'sequential',
    playlist_id TEXT,
    playlist_scope TEXT,
    track_id TEXT,
    position_ms INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS music_queue (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    track_id TEXT,
    title TEXT NOT NULL,
    artist TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual_url',
    source_url TEXT NOT NULL,
    duration_ms INTEGER,
    added_by_user_id TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 10,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(added_by_user_id) REFERENCES users(id) ON DELETE CASCADE
);
