ALTER TABLE users ADD COLUMN bio TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN is_superuser INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN deleted_at INTEGER;

UPDATE users
SET is_superuser = 1
WHERE uid = '66666666'
   OR username_normalized = 'gang'
   OR email_normalized = 'gang-chat@outlook.com';

ALTER TABLE live_participants ADD COLUMN headphones_muted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE live_participants ADD COLUMN voice_blocked INTEGER NOT NULL DEFAULT 0;
