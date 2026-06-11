-- Atomic monotonic counters for human-facing public ids (user uid, room rid).
-- The previous "SELECT MAX(CAST(... AS INTEGER))+1" scheme had a TOCTOU race:
-- two concurrent registrations (or room creates) could read the same MAX and
-- mint the same id, then collide on the unique index. A dedicated counter
-- table lets us allocate ids with a single atomic UPSERT ... RETURNING.
CREATE TABLE IF NOT EXISTS id_sequences (
    name      TEXT PRIMARY KEY NOT NULL,
    next_value INTEGER NOT NULL
);

-- Seed each sequence to one past the current maximum so freshly minted ids
-- never collide with rows created before this migration. The superuser uid
-- (66666666) is intentionally excluded from the users seed, matching the old
-- NextUserUID query that ignored it.
INSERT INTO id_sequences (name, next_value)
VALUES (
    'user_uid',
    (SELECT COALESCE(MAX(CAST(uid AS INTEGER)), 9999999) + 1
       FROM users
      WHERE uid <> '66666666')
)
ON CONFLICT(name) DO NOTHING;

INSERT INTO id_sequences (name, next_value)
VALUES (
    'room_rid',
    (SELECT COALESCE(MAX(CAST(rid AS INTEGER)), 19999999) + 1 FROM rooms)
)
ON CONFLICT(name) DO NOTHING;
