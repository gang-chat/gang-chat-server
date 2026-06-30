WITH duplicate_stickers AS (
    SELECT
        id,
        ROW_NUMBER() OVER (
            PARTITION BY pack_id, name
            ORDER BY created_at ASC, id ASC
        ) AS duplicate_index
    FROM stickers
)
UPDATE stickers
SET name = name || ' (' || id || ')'
WHERE id IN (
    SELECT id FROM duplicate_stickers WHERE duplicate_index > 1
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_stickers_pack_name_unique
    ON stickers(pack_id, name);
