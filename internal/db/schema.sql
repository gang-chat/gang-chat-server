-- The Gang Chat database, version 1: every table the server uses.
--
-- Converted from the MySQL schema the server ran on before, including the
-- production database's unique keys. Times are unix seconds or milliseconds in INTEGER
-- columns, as before; JSON is stored as TEXT. Names that MySQL compared without
-- regard to case (its utf8mb4_0900_ai_ci collation) are COLLATE NOCASE here,
-- so duplicate checks on them behave the same.
--
-- Later changes go in db.go as new migrations, never as edits to this file:
-- databases already on version 1 do not run it again.

CREATE TABLE users (
  id TEXT NOT NULL PRIMARY KEY,
  uid TEXT NOT NULL,
  username TEXT NOT NULL,
  username_normalized TEXT NOT NULL,
  email TEXT NOT NULL,
  email_normalized TEXT NOT NULL,
  password_hash TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  display_name TEXT,
  bio TEXT NOT NULL,
  gender TEXT NOT NULL DEFAULT 'secret',
  avatar_url TEXT,
  default_avatar_key TEXT NOT NULL DEFAULT 'blue-3',
  email_verified INTEGER NOT NULL DEFAULT 1,
  email_public INTEGER NOT NULL DEFAULT 0,
  phone_number TEXT,
  phone_number_normalized TEXT,
  phone_number_public INTEGER NOT NULL DEFAULT 0,
  language TEXT NOT NULL DEFAULT 'zh-Hans',
  is_superuser INTEGER NOT NULL DEFAULT 0,
  username_updated_at INTEGER,
  deleted_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_users_uid ON users (uid);
CREATE UNIQUE INDEX uq_users_username_normalized ON users (username_normalized);
CREATE UNIQUE INDEX uq_users_email_normalized ON users (email_normalized);
-- Accounts without a phone number store '' (older rows) or NULL.
CREATE UNIQUE INDEX uq_users_phone_number_normalized ON users (phone_number_normalized)
  WHERE phone_number_normalized <> '';

CREATE TABLE user_sessions (
  id TEXT NOT NULL PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  refresh_token_hash TEXT NOT NULL,
  user_agent TEXT,
  ip_address TEXT,
  expires_at INTEGER NOT NULL,
  revoked_at INTEGER,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_user_sessions_refresh_token_hash ON user_sessions (refresh_token_hash);
CREATE INDEX idx_user_sessions_user_id ON user_sessions (user_id);
CREATE INDEX idx_user_sessions_expires_at ON user_sessions (expires_at);

CREATE TABLE user_audio_settings (
  user_id TEXT NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  default_audio_input_volume INTEGER NOT NULL DEFAULT 100 CHECK (default_audio_input_volume BETWEEN 0 AND 100),
  default_audio_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (default_audio_output_volume BETWEEN 0 AND 100),
  live_mic_input_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_mic_input_volume BETWEEN 0 AND 100),
  live_voice_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_voice_output_volume BETWEEN 0 AND 100),
  live_screen_share_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_screen_share_output_volume BETWEEN 0 AND 100),
  live_music_output_volume INTEGER NOT NULL DEFAULT 100 CHECK (live_music_output_volume BETWEEN 0 AND 100),
  updated_at INTEGER NOT NULL
);

CREATE TABLE id_sequences (
  name TEXT NOT NULL PRIMARY KEY,
  next_value INTEGER NOT NULL
);

CREATE TABLE idempotency_keys (
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  response_status INTEGER NOT NULL,
  response_body TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, method, path, idempotency_key)
);
CREATE INDEX idx_idempotency_keys_created_at ON idempotency_keys (created_at);

CREATE TABLE email_verification_challenges (
  id TEXT NOT NULL PRIMARY KEY,
  email TEXT NOT NULL,
  email_normalized TEXT NOT NULL,
  code_hash TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  resend_available_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  verified_at INTEGER,
  verification_token_hash TEXT,
  verification_token_expires_at INTEGER,
  consumed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_email_verification_token ON email_verification_challenges (verification_token_hash);
CREATE INDEX idx_email_verification_email_created ON email_verification_challenges (email_normalized, created_at);

CREATE TABLE email_verification_locks (
  email_normalized TEXT NOT NULL PRIMARY KEY,
  updated_at INTEGER NOT NULL
);

CREATE TABLE password_reset_challenges (
  id TEXT NOT NULL PRIMARY KEY,
  user_id TEXT NOT NULL,
  email TEXT NOT NULL,
  code_hash TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  resend_available_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  verified_at INTEGER,
  reset_token_hash TEXT,
  reset_token_expires_at INTEGER,
  consumed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_password_reset_token ON password_reset_challenges (reset_token_hash);
CREATE INDEX idx_password_reset_user_created ON password_reset_challenges (user_id, created_at);

CREATE TABLE password_reset_session_grants (
  session_id TEXT NOT NULL PRIMARY KEY,
  user_id TEXT NOT NULL,
  email_normalized TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_password_reset_grant_user ON password_reset_session_grants (user_id);

CREATE TABLE push_devices (
  provider TEXT NOT NULL,
  installation_id TEXT NOT NULL,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  session_id TEXT NOT NULL REFERENCES user_sessions (id) ON DELETE CASCADE,
  token TEXT NOT NULL,
  platform TEXT NOT NULL DEFAULT 'android',
  notifications_enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (provider, installation_id)
);
CREATE INDEX idx_push_devices_user ON push_devices (user_id, notifications_enabled);
CREATE INDEX idx_push_devices_session ON push_devices (session_id);

CREATE TABLE assets (
  id TEXT NOT NULL PRIMARY KEY,
  owner_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
  purpose TEXT NOT NULL,
  filename TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  url TEXT NOT NULL,
  storage_key TEXT,
  thumbnail_url TEXT,
  width INTEGER,
  height INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_assets_owner ON assets (owner_user_id);

CREATE TABLE rooms (
  id TEXT NOT NULL PRIMARY KEY,
  rid TEXT NOT NULL,
  name TEXT NOT NULL,
  avatar_asset_id TEXT,
  avatar_url TEXT,
  default_avatar_key TEXT NOT NULL,
  created_by_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
  visibility TEXT NOT NULL DEFAULT 'public',
  join_policy TEXT NOT NULL DEFAULT 'open',
  ai_voice_announce_enabled INTEGER NOT NULL DEFAULT 1,
  gang_bot_enabled INTEGER NOT NULL DEFAULT 1,
  message_recall_policy TEXT NOT NULL DEFAULT 'time_limited',
  message_recall_window_seconds INTEGER DEFAULT 120,
  description TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_rooms_rid ON rooms (rid);
CREATE INDEX fk_rooms_creator ON rooms (created_by_user_id);

CREATE TABLE room_memberships (
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  role TEXT NOT NULL DEFAULT 'member',
  remark_name TEXT,
  room_display_name TEXT,
  room_avatar_url TEXT,
  room_default_avatar_key TEXT,
  notification_level TEXT NOT NULL DEFAULT 'all',
  text_muted_until INTEGER,
  is_pinned INTEGER NOT NULL DEFAULT 0,
  joined_at INTEGER NOT NULL,
  PRIMARY KEY (room_id, user_id)
);
CREATE INDEX idx_room_memberships_user_id ON room_memberships (user_id);
CREATE INDEX idx_room_memberships_user_pinned ON room_memberships (user_id, is_pinned);

CREATE TABLE room_blacklist (
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  blocked_by_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (room_id, user_id)
);
CREATE INDEX idx_room_blacklist_user ON room_blacklist (user_id, room_id);
CREATE INDEX fk_room_blacklist_actor ON room_blacklist (blocked_by_user_id);

CREATE TABLE room_invites (
  id TEXT NOT NULL PRIMARY KEY,
  room_id TEXT NOT NULL,
  inviter_user_id TEXT NOT NULL,
  target_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  room_rid TEXT NOT NULL DEFAULT '',
  room_name TEXT NOT NULL DEFAULT '',
  room_avatar_url TEXT,
  room_default_avatar_key TEXT NOT NULL DEFAULT 'room-1',
  room_visibility TEXT NOT NULL DEFAULT 'private',
  room_join_policy TEXT NOT NULL DEFAULT 'closed'
);
CREATE UNIQUE INDEX uq_room_invites_room_target_inviter ON room_invites (room_id, target_user_id, inviter_user_id);
CREATE INDEX idx_room_invites_target ON room_invites (target_user_id, status, created_at);
CREATE INDEX idx_room_invites_room_target ON room_invites (room_id, target_user_id, status);

CREATE TABLE join_requests (
  id TEXT NOT NULL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'pending',
  reason TEXT NOT NULL,
  reviewer_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
  reviewed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_join_requests_room_user ON join_requests (room_id, user_id);
CREATE INDEX fk_join_requests_user ON join_requests (user_id);
CREATE INDEX fk_join_requests_reviewer ON join_requests (reviewer_user_id);

CREATE TABLE room_voice_bans (
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  created_by_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  reason TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  mic_blocked INTEGER NOT NULL DEFAULT 1,
  headphones_blocked INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (room_id, user_id)
);
CREATE INDEX idx_room_voice_bans_room ON room_voice_bans (room_id);
CREATE INDEX fk_room_voice_bans_user ON room_voice_bans (user_id);
CREATE INDEX fk_room_voice_bans_actor ON room_voice_bans (created_by_user_id);

CREATE TABLE room_ai_voice_preferences (
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  enabled INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (room_id, user_id)
);

CREATE TABLE messages (
  id TEXT NOT NULL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  sender_user_id TEXT NOT NULL,
  client_message_id TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'text',
  body TEXT NOT NULL,
  mentions_json TEXT NOT NULL,
  attachments_json TEXT NOT NULL,
  quote_json TEXT,
  is_recalled INTEGER NOT NULL DEFAULT 0,
  recalled_at INTEGER,
  recalled_by_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
  is_force_deleted INTEGER NOT NULL DEFAULT 0,
  force_deleted_at INTEGER,
  force_deleted_by_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL,
  sender_uid_snapshot TEXT NOT NULL DEFAULT '',
  sender_username_snapshot TEXT NOT NULL DEFAULT '',
  sender_display_name_snapshot TEXT,
  sender_room_display_name_snapshot TEXT,
  sender_avatar_url_snapshot TEXT,
  sender_default_avatar_key_snapshot TEXT NOT NULL DEFAULT 'blue-3',
  sender_is_superuser_snapshot INTEGER NOT NULL DEFAULT 0,
  sender_room_role_snapshot TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX uq_messages_client ON messages (room_id, sender_user_id, client_message_id);
CREATE INDEX idx_messages_room_created ON messages (room_id, created_at, id);
CREATE INDEX fk_messages_sender ON messages (sender_user_id);
CREATE INDEX fk_messages_recalled_by ON messages (recalled_by_user_id);
CREATE INDEX fk_messages_force_deleted_by ON messages (force_deleted_by_user_id);

CREATE TABLE message_history_deletions (
  user_id TEXT NOT NULL,
  message_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, message_id)
);
CREATE INDEX idx_message_history_deletions_message ON message_history_deletions (message_id);

CREATE TABLE message_recall_requests (
  id TEXT NOT NULL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
  requested_by_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_message_recall_request ON message_recall_requests (room_id, message_id, requested_by_user_id);
CREATE INDEX idx_message_recall_requests_room_status ON message_recall_requests (room_id, status, created_at);
CREATE INDEX fk_message_recall_requests_message ON message_recall_requests (message_id);
CREATE INDEX fk_message_recall_requests_requester ON message_recall_requests (requested_by_user_id);

CREATE TABLE room_reads (
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  last_read_message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (room_id, user_id)
);
CREATE INDEX idx_room_reads_message ON room_reads (last_read_message_id);
CREATE INDEX fk_room_reads_user ON room_reads (user_id);

CREATE TABLE room_notifications (
  id TEXT NOT NULL PRIMARY KEY,
  recipient_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  room_id TEXT NOT NULL,
  actor_user_id TEXT,
  type TEXT NOT NULL,
  from_role TEXT,
  to_role TEXT,
  message_id TEXT,
  message_body_preview TEXT,
  created_at INTEGER NOT NULL,
  read_at INTEGER,
  room_rid TEXT NOT NULL DEFAULT '',
  room_name TEXT NOT NULL DEFAULT '',
  room_avatar_url TEXT,
  room_default_avatar_key TEXT NOT NULL DEFAULT 'room-1',
  room_visibility TEXT NOT NULL DEFAULT 'private',
  room_join_policy TEXT NOT NULL DEFAULT 'closed',
  room_description TEXT NOT NULL,
  room_created_by_user_id TEXT,
  actor_uid TEXT,
  actor_username TEXT,
  actor_display_name TEXT,
  actor_avatar_url TEXT,
  actor_default_avatar_key TEXT NOT NULL DEFAULT 'blue-3',
  actor_room_display_name TEXT,
  actor_room_role TEXT
);
CREATE INDEX idx_room_notifications_recipient_created ON room_notifications (recipient_user_id, created_at);
CREATE INDEX idx_room_notifications_room_created ON room_notifications (room_id, created_at);

CREATE TABLE room_notification_deletions (
  user_id TEXT NOT NULL,
  notification_type TEXT NOT NULL,
  notification_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, notification_type, notification_id)
);

CREATE TABLE live_participants (
  live_session_id TEXT NOT NULL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  client_live_session_id TEXT NOT NULL,
  joined_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  mic_muted INTEGER NOT NULL DEFAULT 1,
  mic_blocked INTEGER NOT NULL DEFAULT 0,
  headphones_muted INTEGER NOT NULL DEFAULT 0,
  headphones_blocked INTEGER NOT NULL DEFAULT 0,
  voice_blocked INTEGER NOT NULL DEFAULT 0,
  camera_on INTEGER NOT NULL DEFAULT 0,
  camera_mirrored INTEGER NOT NULL DEFAULT 0,
  screen_sharing INTEGER NOT NULL DEFAULT 0,
  watching_screen_user_id TEXT,
  connection_state TEXT NOT NULL DEFAULT 'joining'
);
CREATE UNIQUE INDEX uq_live_participants_room_user ON live_participants (room_id, user_id);
CREATE INDEX idx_live_participants_room ON live_participants (room_id);
CREATE INDEX idx_live_participants_watching_screen ON live_participants (room_id, watching_screen_user_id, connection_state);
CREATE INDEX fk_live_participants_user ON live_participants (user_id);

CREATE TABLE live_member_volumes (
  room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
  listener_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  target_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  volume INTEGER NOT NULL DEFAULT 100 CHECK (volume BETWEEN 0 AND 100),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (room_id, listener_user_id, target_user_id),
  CHECK (listener_user_id <> target_user_id)
);
CREATE INDEX idx_live_member_volumes_listener ON live_member_volumes (listener_user_id, room_id);
CREATE INDEX fk_live_member_volumes_target ON live_member_volumes (target_user_id);

CREATE TABLE sticker_packs (
  id TEXT NOT NULL PRIMARY KEY,
  owner_user_id TEXT REFERENCES users (id) ON DELETE CASCADE,
  room_id TEXT REFERENCES rooms (id) ON DELETE CASCADE,
  scope TEXT NOT NULL,
  name TEXT NOT NULL COLLATE NOCASE,
  sort_order INTEGER NOT NULL DEFAULT 10,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_sticker_packs_owner_name ON sticker_packs (scope, owner_user_id, name);
CREATE UNIQUE INDEX uq_sticker_packs_room_name ON sticker_packs (scope, room_id, name);
CREATE INDEX idx_sticker_packs_owner ON sticker_packs (owner_user_id);
CREATE INDEX idx_sticker_packs_room ON sticker_packs (room_id);

CREATE TABLE stickers (
  id TEXT NOT NULL PRIMARY KEY,
  pack_id TEXT NOT NULL REFERENCES sticker_packs (id) ON DELETE CASCADE,
  asset_id TEXT NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  sort_order INTEGER NOT NULL DEFAULT 10,
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_stickers_pack_name ON stickers (pack_id, name);
CREATE INDEX idx_stickers_pack ON stickers (pack_id);
CREATE INDEX idx_stickers_asset ON stickers (asset_id);

CREATE TABLE sticker_asset_lifecycle (
  asset_id TEXT NOT NULL PRIMARY KEY,
  expires_at INTEGER,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_sticker_asset_lifecycle_expires_at ON sticker_asset_lifecycle (expires_at);

CREATE TABLE music_tracks (
  id TEXT NOT NULL PRIMARY KEY,
  source TEXT NOT NULL,
  external_track_id TEXT NOT NULL,
  title TEXT NOT NULL,
  artist TEXT NOT NULL DEFAULT '',
  artists_json TEXT NOT NULL,
  duration_ms INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_music_tracks_source_external ON music_tracks (source, external_track_id);

CREATE TABLE music_playlists (
  id TEXT NOT NULL PRIMARY KEY,
  scope_type TEXT NOT NULL,
  owner_user_id TEXT REFERENCES users (id) ON DELETE CASCADE,
  room_id TEXT REFERENCES rooms (id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  description TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX uq_music_playlists_user_name ON music_playlists (owner_user_id, name);
CREATE INDEX idx_music_playlists_user_order ON music_playlists (owner_user_id, sort_order, created_at);
CREATE INDEX idx_music_playlists_room_order ON music_playlists (room_id, sort_order, created_at);

CREATE TABLE music_playlist_items (
  id TEXT NOT NULL PRIMARY KEY,
  playlist_id TEXT NOT NULL REFERENCES music_playlists (id) ON DELETE CASCADE,
  track_id TEXT NOT NULL REFERENCES music_tracks (id) ON DELETE RESTRICT,
  added_by_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  sort_order INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_music_playlist_items_order ON music_playlist_items (playlist_id, sort_order, created_at, id);
CREATE INDEX idx_music_playlist_items_track ON music_playlist_items (track_id);
CREATE INDEX idx_music_playlist_items_adder ON music_playlist_items (added_by_user_id);

CREATE TABLE room_music_box_queue (
  id TEXT NOT NULL PRIMARY KEY,
  room_id TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'netease',
  track_id TEXT NOT NULL,
  title TEXT NOT NULL,
  artist TEXT NOT NULL,
  duration_ms INTEGER,
  status TEXT NOT NULL DEFAULT 'pending',
  file_path TEXT,
  file_size_bytes INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  added_by_user_id TEXT NOT NULL,
  queue_scope TEXT NOT NULL DEFAULT 'temporary',
  snapshot_id TEXT,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_room_music_box_queue_room_order ON room_music_box_queue (room_id, queue_scope, sort_order);

CREATE TABLE room_music_box_state (
  room_id TEXT NOT NULL PRIMARY KEY,
  state TEXT NOT NULL DEFAULT 'stopped',
  current_item_id TEXT,
  position_ms INTEGER NOT NULL DEFAULT 0,
  volume INTEGER NOT NULL DEFAULT 100,
  revision INTEGER NOT NULL DEFAULT 0,
  playback_mode TEXT NOT NULL DEFAULT 'sequential',
  active_source_type TEXT NOT NULL DEFAULT 'temporary',
  active_playlist_id TEXT,
  active_playlist_name TEXT,
  active_playlist_owner_id TEXT,
  active_playlist_created_at INTEGER NOT NULL DEFAULT 0,
  active_snapshot_id TEXT,
  play_cursor_scope TEXT,
  play_cursor_snapshot_id TEXT,
  play_cursor_after_sort INTEGER NOT NULL DEFAULT -1,
  updated_at INTEGER NOT NULL
);

-- Client releases, maintained in the admin console. The homepage lists the
-- published ones (notes are its announcements) and /app/version reports the
-- newest. release_date is YYYY-MM-DD; download URLs are empty when a platform
-- has no installer for that version.
CREATE TABLE app_releases (
  version TEXT NOT NULL PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  release_date TEXT NOT NULL DEFAULT '',
  published INTEGER NOT NULL DEFAULT 0,
  minimum_supported_version TEXT NOT NULL DEFAULT '',
  windows_url TEXT NOT NULL DEFAULT '',
  macos_url TEXT NOT NULL DEFAULT '',
  android_url TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Sign-ins to the admin console, which has one account set in config.json
-- (admin_username and admin_password). Only a SHA-256 of each token is kept.
-- Times are unix milliseconds.
CREATE TABLE admin_sessions (
  id TEXT NOT NULL PRIMARY KEY,
  token_hash TEXT NOT NULL,
  username TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE UNIQUE INDEX uq_admin_sessions_token_hash ON admin_sessions (token_hash);

-- What the admin console changed, or tried to (unix milliseconds).
CREATE TABLE admin_audit_log (
  id INTEGER PRIMARY KEY,
  created_at INTEGER NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_admin_audit_log_created_at ON admin_audit_log (created_at);
