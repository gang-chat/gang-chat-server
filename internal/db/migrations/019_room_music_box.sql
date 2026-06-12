-- Room-level server-side music box.
--
-- Distinct from the existing music_* tables (those drive client-coordinated
-- playback where each listener plays locally and syncs state). Here the server
-- downloads + transcodes each track to Opus and broadcasts a single audio
-- track into the room's LiveKit session via a bot participant.

CREATE TABLE IF NOT EXISTS room_music_box_queue (
    id               TEXT PRIMARY KEY NOT NULL,
    room_id          TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'netease',
    track_id         TEXT NOT NULL,
    title            TEXT NOT NULL,
    artist           TEXT NOT NULL DEFAULT '',
    duration_ms      INTEGER,
    -- pending -> downloading -> ready, or failed
    status           TEXT NOT NULL DEFAULT 'pending',
    file_path        TEXT,
    file_size_bytes  INTEGER NOT NULL DEFAULT 0,
    error            TEXT,
    added_by_user_id TEXT NOT NULL,
    sort_order       INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(added_by_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_room_music_box_queue_room
ON room_music_box_queue(room_id, sort_order, created_at);

CREATE INDEX IF NOT EXISTS idx_room_music_box_queue_status
ON room_music_box_queue(room_id, status);

CREATE TABLE IF NOT EXISTS room_music_box_state (
    room_id         TEXT PRIMARY KEY NOT NULL,
    -- stopped | playing | paused
    state           TEXT NOT NULL DEFAULT 'stopped',
    current_item_id TEXT,
    position_ms     INTEGER NOT NULL DEFAULT 0,
    volume          INTEGER NOT NULL DEFAULT 100 CHECK (volume BETWEEN 0 AND 100),
    updated_at      INTEGER NOT NULL,
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(current_item_id) REFERENCES room_music_box_queue(id) ON DELETE SET NULL
);
