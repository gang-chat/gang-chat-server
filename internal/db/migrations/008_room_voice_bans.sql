-- Persistent, room-scoped voice bans.
--
-- Previously a "voice block" lived only in the live_participants row, which is
-- deleted the moment the user disconnects (client leave or LiveKit RTCP
-- timeout). That meant a ban silently evaporated on reconnect — a banned user
-- could rejoin and speak again. Voice bans are a moderation policy that must
-- outlive any single live session, so they get their own table.
--
-- Token issuance and the self-serve mic guard both consult this table, and a
-- (re)join re-applies the ban to the fresh participant row, so the ban holds
-- across leave/rejoin until an admin explicitly restores voice.
CREATE TABLE IF NOT EXISTS room_voice_bans (
    room_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    created_by_user_id TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY(room_id, user_id),
    FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(created_by_user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_room_voice_bans_room ON room_voice_bans(room_id);

-- Backfill from any voice blocks currently encoded in live participant rows so
-- existing blocks survive the migration. created_by is unknown for historical
-- rows; attribute to the room owner.
INSERT OR IGNORE INTO room_voice_bans (room_id, user_id, created_by_user_id, reason, created_at)
SELECT lp.room_id,
       lp.user_id,
       COALESCE(r.created_by_user_id, lp.user_id),
       '',
       lp.updated_at
FROM live_participants lp
JOIN rooms r ON r.id = lp.room_id
WHERE lp.voice_blocked = 1;
