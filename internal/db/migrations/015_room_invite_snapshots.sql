CREATE TABLE room_invites_new (
    id TEXT PRIMARY KEY NOT NULL,
    room_id TEXT NOT NULL,
    inviter_user_id TEXT NOT NULL,
    target_user_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    room_rid TEXT NOT NULL DEFAULT '',
    room_name TEXT NOT NULL DEFAULT '',
    room_avatar_url TEXT,
    room_default_avatar_key TEXT NOT NULL DEFAULT 'room-1',
    room_visibility TEXT NOT NULL DEFAULT 'private',
    room_join_policy TEXT NOT NULL DEFAULT 'closed',
    UNIQUE(room_id, target_user_id),
    FOREIGN KEY(inviter_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(target_user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO room_invites_new (
    id,
    room_id,
    inviter_user_id,
    target_user_id,
    status,
    created_at,
    updated_at,
    room_rid,
    room_name,
    room_avatar_url,
    room_default_avatar_key,
    room_visibility,
    room_join_policy
)
SELECT
    ri.id,
    ri.room_id,
    ri.inviter_user_id,
    ri.target_user_id,
    ri.status,
    ri.created_at,
    ri.updated_at,
    COALESCE(r.rid, ''),
    COALESCE(r.name, '已删除房间'),
    r.avatar_url,
    COALESCE(r.default_avatar_key, 'room-1'),
    COALESCE(r.visibility, 'private'),
    COALESCE(r.join_policy, 'closed')
FROM room_invites ri
LEFT JOIN rooms r ON r.id = ri.room_id;

DROP TABLE room_invites;
ALTER TABLE room_invites_new RENAME TO room_invites;

CREATE INDEX IF NOT EXISTS idx_room_invites_target
ON room_invites(target_user_id, status, created_at);

