ALTER TABLE room_voice_bans ADD COLUMN mic_blocked INTEGER NOT NULL DEFAULT 1;
ALTER TABLE room_voice_bans ADD COLUMN headphones_blocked INTEGER NOT NULL DEFAULT 1;
