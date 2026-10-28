package config

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
)

const (
	DefaultAssetUploadMaxBytes                int64 = 50 * 1024 * 1024
	DefaultImageUploadMaxBytes                int64 = 10 * 1024 * 1024
	DefaultStickerAssetOrphanTTLSeconds       int64 = 30 * 24 * 60 * 60
	DefaultStickerAssetCleanupIntervalSeconds int64 = 60 * 60
	DefaultMusicBoxMaxBytesPerRoom            int64 = 200 * 1024 * 1024
	DefaultMusicBoxCacheMaxBytes              int64 = 2 * 1024 * 1024 * 1024
	DefaultMusicBoxEmptyRoomGraceSeconds      int64 = 10 * 60
)

type Config struct {
	Bind                               string `json:"bind"`
	DatabasePath                       string `json:"database_path"`
	JWTSecret                          string `json:"jwt_secret"`
	AccessTokenTTLSeconds              int64  `json:"access_token_ttl_seconds"`
	RefreshTokenTTLSeconds             int64  `json:"refresh_token_ttl_seconds"`
	LoginMaxAttempts                   int    `json:"login_max_attempts"`
	LoginWindowSeconds                 int64  `json:"login_window_seconds"`
	AssetUploadMaxBytes                int64  `json:"asset_upload_max_bytes"`
	ImageUploadMaxBytes                int64  `json:"image_upload_max_bytes"`
	StickerAssetOrphanTTLSeconds       int64  `json:"sticker_asset_orphan_ttl_seconds"`
	StickerAssetCleanupIntervalSeconds int64  `json:"sticker_asset_cleanup_interval_seconds"`
	// StoragePath is the directory for uploaded files (assets/) and the client
	// releases that release sync copies from GitHub (assets/releases/).
	StoragePath                   string   `json:"storage_path"`
	GeoIPDatabasePath             string   `json:"geoip_db_path"`
	TrustedProxies                []string `json:"trusted_proxies"`
	AllowedOrigins                []string `json:"allowed_origins"`
	LiveKitHost                   string   `json:"livekit_host"`
	LiveKitAPIKey                 string   `json:"livekit_api_key"`
	LiveKitAPISecret              string   `json:"livekit_api_secret"`
	FFmpegPath                    string   `json:"ffmpeg_path"`
	MusicBoxDir                   string   `json:"music_box_dir"`
	MusicBoxMaxBytesPerRoom       int64    `json:"music_box_max_bytes_per_room"`
	MusicBoxCacheMaxBytes         int64    `json:"music_box_cache_max_bytes"`
	MusicBoxOpusBitrate           string   `json:"music_box_opus_bitrate"`
	MusicBoxTranscodeWorkers      int      `json:"music_box_transcode_workers"`
	MusicBoxDownloadBitrate       string   `json:"music_box_download_bitrate"`
	MusicBoxCompactProgressOnly   bool     `json:"music_box_compact_progress_only"`
	MusicBoxEmptyRoomGraceSeconds int64    `json:"music_box_empty_room_grace_seconds"`

	// gang-bot, the LLM member of every room, calls an Anthropic Messages API
	// endpoint. All three are required to turn it on: leave them all empty to
	// keep the bot off. The URL is the API root, e.g. https://api.anthropic.com
	// or a compatible proxy.
	GangBotAPIURL string `json:"gang_bot_api_url"`
	GangBotAPIKey string `json:"gang_bot_api_key"`
	GangBotModel  string `json:"gang_bot_model"`

	// The admin console (gang-chat-admin) signs in with this one account. Both
	// must be set, and the password must pass the console's password policy,
	// or the console's API stays off.
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`

	// Release sync copies a GitHub release's installers and update feeds into
	// storage_path's assets/releases/, renamed <release_sync_asset_prefix>_<tag>.<ext>,
	// on its own once the release workflow has succeeded, or from the admin
	// console. Leave the repository empty to turn it off; the token is only
	// needed for private repositories or a higher rate limit.
	// release_download_base_url is where the server's /assets/releases is
	// public, such as https://ky-z.com/gang-chat/assets/releases; the homepage
	// links the installers there.
	ReleaseSyncRepositoryURL string `json:"release_sync_repository_url"`
	ReleaseSyncAssetPrefix   string `json:"release_sync_asset_prefix"`
	ReleaseSyncGitHubToken   string `json:"release_sync_github_token"`
	ReleaseDownloadBaseURL   string `json:"release_download_base_url"`

	ResendAPIBaseURL string `json:"resend_api_base_url"`
	ResendAPIKey     string `json:"resend_api_key"`
	EmailFrom        string `json:"email_from"`

	// Android offline push is optional. When these values are empty the
	// registration API remains available, but the FCM sender is disabled.
	// Keeping provider credentials out of the chat package lets a vendor push
	// implementation (for example OPPO Push) be added without touching message
	// persistence or online-presence semantics.
	FCMProjectID          string `json:"fcm_project_id"`
	FCMServiceAccountFile string `json:"fcm_service_account_file"`
}

func parseList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func Load() *Config {
	path := configPathFromArgs(os.Args[1:])
	flag.StringVar(&path, "config", path, "config JSON path")

	raw, err := os.ReadFile(path)
	if err != nil {
		panic("read config " + path + ": " + err.Error())
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		panic("parse config " + path + ": " + err.Error())
	}

	trustedProxies := strings.Join(cfg.TrustedProxies, ",")
	flag.StringVar(&cfg.Bind, "bind", cfg.Bind, "listen address")
	flag.StringVar(&cfg.JWTSecret, "jwt-secret", cfg.JWTSecret, "JWT signing secret")
	flag.StringVar(&cfg.DatabasePath, "database-path", cfg.DatabasePath, "SQLite database file")
	flag.Int64Var(&cfg.AssetUploadMaxBytes, "asset-upload-max-bytes", cfg.AssetUploadMaxBytes, "maximum uploaded file size in bytes")
	flag.Int64Var(&cfg.ImageUploadMaxBytes, "image-upload-max-bytes", cfg.ImageUploadMaxBytes, "maximum uploaded image size in bytes")
	flag.Int64Var(&cfg.StickerAssetOrphanTTLSeconds, "sticker-asset-orphan-ttl-seconds", cfg.StickerAssetOrphanTTLSeconds, "seconds an unreferenced sticker asset is retained")
	flag.Int64Var(&cfg.StickerAssetCleanupIntervalSeconds, "sticker-asset-cleanup-interval-seconds", cfg.StickerAssetCleanupIntervalSeconds, "seconds between sticker asset cleanup passes")
	flag.StringVar(&cfg.StoragePath, "storage-path", cfg.StoragePath, "directory for uploaded files and client releases")
	flag.StringVar(&cfg.GeoIPDatabasePath, "geoip-db", cfg.GeoIPDatabasePath, "MaxMind GeoIP database path")
	flag.StringVar(&cfg.FFmpegPath, "ffmpeg-path", cfg.FFmpegPath, "path to the ffmpeg binary used for music transcoding")
	flag.StringVar(&cfg.MusicBoxDir, "music-box-dir", cfg.MusicBoxDir, "directory for transcoded room music files")
	flag.Int64Var(&cfg.MusicBoxMaxBytesPerRoom, "music-box-max-bytes-per-room", cfg.MusicBoxMaxBytesPerRoom, "max on-disk bytes of transcoded music per room")
	flag.Int64Var(&cfg.MusicBoxCacheMaxBytes, "music-box-cache-max-bytes", cfg.MusicBoxCacheMaxBytes, "global max bytes for shared broadcast music cache")
	flag.StringVar(&cfg.MusicBoxOpusBitrate, "music-box-opus-bitrate", cfg.MusicBoxOpusBitrate, "Opus bitrate for broadcast transcode, e.g. 128k")
	flag.IntVar(&cfg.MusicBoxTranscodeWorkers, "music-box-transcode-workers", cfg.MusicBoxTranscodeWorkers, "max concurrent transcode jobs")
	flag.StringVar(&cfg.MusicBoxDownloadBitrate, "music-box-download-bitrate", cfg.MusicBoxDownloadBitrate, "GD download quality (128/192/320/740/999)")
	flag.BoolVar(&cfg.MusicBoxCompactProgressOnly, "music-box-compact-progress-only", cfg.MusicBoxCompactProgressOnly, "publish compact music progress heartbeats without legacy full snapshots")
	flag.Int64Var(&cfg.MusicBoxEmptyRoomGraceSeconds, "music-box-empty-room-grace-seconds", cfg.MusicBoxEmptyRoomGraceSeconds, "seconds an empty voice room keeps its temporary music state")
	flag.StringVar(&cfg.GangBotAPIURL, "gang-bot-api-url", cfg.GangBotAPIURL, "Anthropic Messages API root for gang-bot")
	flag.StringVar(&cfg.GangBotAPIKey, "gang-bot-api-key", cfg.GangBotAPIKey, "API key for gang-bot")
	flag.StringVar(&cfg.GangBotModel, "gang-bot-model", cfg.GangBotModel, "model name for gang-bot")
	flag.StringVar(&cfg.ResendAPIBaseURL, "resend-api-base-url", cfg.ResendAPIBaseURL, "Resend API base URL")
	flag.StringVar(&cfg.ResendAPIKey, "resend-api-key", cfg.ResendAPIKey, "Resend API key")
	flag.StringVar(&cfg.EmailFrom, "email-from", cfg.EmailFrom, "sender used for account emails")
	flag.StringVar(&cfg.FCMProjectID, "fcm-project-id", cfg.FCMProjectID, "Firebase project ID used for Android offline push")
	flag.StringVar(&cfg.FCMServiceAccountFile, "fcm-service-account-file", cfg.FCMServiceAccountFile, "path to a Firebase service-account JSON file")
	flag.StringVar(&trustedProxies, "trusted-proxies", trustedProxies, "comma-separated trusted proxy IPs/CIDRs")
	allowedOrigins := strings.Join(cfg.AllowedOrigins, ",")
	flag.StringVar(&allowedOrigins, "allowed-origins", allowedOrigins, "comma-separated allowed CORS origins, or * for any")
	flag.Parse()
	if cfg.StickerAssetOrphanTTLSeconds <= 0 {
		cfg.StickerAssetOrphanTTLSeconds = DefaultStickerAssetOrphanTTLSeconds
	}
	if cfg.StickerAssetCleanupIntervalSeconds <= 0 {
		cfg.StickerAssetCleanupIntervalSeconds = DefaultStickerAssetCleanupIntervalSeconds
	}
	if cfg.MusicBoxEmptyRoomGraceSeconds <= 0 {
		cfg.MusicBoxEmptyRoomGraceSeconds = DefaultMusicBoxEmptyRoomGraceSeconds
	}
	if cfg.MusicBoxCacheMaxBytes <= 0 {
		cfg.MusicBoxCacheMaxBytes = DefaultMusicBoxCacheMaxBytes
	}
	cfg.TrustedProxies = parseList(trustedProxies)
	cfg.AllowedOrigins = parseList(allowedOrigins)

	if cfg.JWTSecret == "" {
		panic("jwt_secret is required in config.json")
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		panic("database_path (the SQLite database file) is required in config.json; " +
			"database_url (MySQL) is no longer used")
	}
	if strings.TrimSpace(cfg.StoragePath) == "" {
		panic("storage_path (the directory for uploaded files and client releases) is required in config.json; " +
			"the s3_* settings are no longer used")
	}
	if missing := cfg.GangBotMissingSettings(); len(missing) > 0 && len(missing) < 3 {
		panic("gang-bot needs all of gang_bot_api_url, gang_bot_api_key and gang_bot_model; missing: " + strings.Join(missing, ", "))
	}

	return &cfg
}

// GangBotEnabled reports whether every gang-bot setting is filled in.
func (c *Config) GangBotEnabled() bool {
	return len(c.GangBotMissingSettings()) == 0
}

// GangBotMissingSettings lists the empty gang-bot settings by config key.
func (c *Config) GangBotMissingSettings() []string {
	var missing []string
	for _, setting := range []struct{ key, value string }{
		{"gang_bot_api_url", c.GangBotAPIURL},
		{"gang_bot_api_key", c.GangBotAPIKey},
		{"gang_bot_model", c.GangBotModel},
	} {
		if strings.TrimSpace(setting.value) == "" {
			missing = append(missing, setting.key)
		}
	}
	return missing
}

func configPathFromArgs(args []string) string {
	for i, arg := range args {
		if arg == "-config" || arg == "--config" {
			if i+1 < len(args) && args[i+1] != "" {
				return args[i+1]
			}
			return "config.json"
		}
		if strings.HasPrefix(arg, "-config=") {
			return strings.TrimPrefix(arg, "-config=")
		}
		if strings.HasPrefix(arg, "--config=") {
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return "config.json"
}
