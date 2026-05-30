CREATE TABLE IF NOT EXISTS user_audio_settings (
    user_id TEXT PRIMARY KEY NOT NULL,
    default_audio_input_volume INTEGER NOT NULL DEFAULT 100 CHECK (default_audio_input_volume BETWEEN 0 AND 100),
    default_audio_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (default_audio_output_volume BETWEEN 0 AND 100),
    live_mic_input_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_mic_input_volume BETWEEN 0 AND 100),
    live_voice_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_voice_output_volume BETWEEN 0 AND 100),
    live_screen_share_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_screen_share_output_volume BETWEEN 0 AND 100),
    live_music_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_music_output_volume BETWEEN 0 AND 100),
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
