-- Drop the legacy client-coordinated music feature.
--
-- These tables backed the old "/music/*" and "/playlists/*" feature where each
-- client played a track locally and synced playback state through the server
-- (music_playback / music_queue / music_sessions / music_invites) plus saved
-- playlists (playlists / playlist_tracks). The app now uses the server-side
-- music box (room_music_box_* from migration 019), which transcodes and
-- broadcasts a single shared Opus stream, so none of these are read or written
-- anymore. Their handlers, routes, and request types have been removed.
--
-- Dropped in FK-dependency order (referencing tables before referenced ones):
--   music_invites    -> music_sessions
--   music_sessions   -> music_queue
--   music_queue / music_playback / playlist_tracks -> playlists
DROP TABLE IF EXISTS music_invites;
DROP TABLE IF EXISTS music_sessions;
DROP TABLE IF EXISTS music_queue;
DROP TABLE IF EXISTS music_playback;
DROP TABLE IF EXISTS playlist_tracks;
DROP TABLE IF EXISTS playlists;
