package config

import (
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultAssetUploadMaxBytes int64 = 50 * 1024 * 1024
	DefaultImageUploadMaxBytes int64 = 10 * 1024 * 1024
	// DefaultMusicBoxMaxBytesPerRoom caps the on-disk size of a single room's
	// transcoded music queue. The queue is bounded by total bytes, not item
	// count, so this is the real backpressure knob.
	DefaultMusicBoxMaxBytesPerRoom int64 = 200 * 1024 * 1024
)

type Config struct {
	Bind                   string
	DatabaseURL            string
	JWTSecret              string
	AccessTokenTTLSeconds  int64
	RefreshTokenTTLSeconds int64
	LoginMaxAttempts       int
	LoginWindowSeconds     int64
	AssetDir               string
	StorageBackend         string
	AssetPublicBaseURL     string
	AssetObjectPrefix      string
	AssetCacheControl      string
	AssetUploadMaxBytes    int64
	ImageUploadMaxBytes    int64
	COSBucket              string
	COSRegion              string
	COSBucketURL           string
	COSSecretID            string
	COSSecretKey           string
	COSSessionToken        string
	COSObjectACL           string
	GeoIPDatabasePath      string
	TrustedProxies         []string
	AllowedOrigins         []string
	LiveKitHost            string
	LiveKitAPIKey          string
	LiveKitAPISecret       string
	FFmpegPath               string
	MusicBoxDir              string
	MusicBoxMaxBytesPerRoom  int64
	MusicBoxOpusBitrate      string
	MusicBoxTranscodeWorkers int
	MusicBoxSource           string
	MusicBoxSourceBitrate    string

	// QQMusic is the optional self-hosted QQ音乐 API integration. It's nil when
	// no config file is present (feature off); non-nil and validated when one is.
	QQMusic *QQMusicConfig
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envListOr(key string, fallback []string) []string {
	if v := os.Getenv(key); v != "" {
		return parseList(v)
	}
	return fallback
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
	cfg := &Config{
		Bind:                   envOr("GANG_BIND", "127.0.0.1:21116"),
		DatabaseURL:            envOr("GANG_DATABASE_URL", "gang-chat.db"),
		JWTSecret:              envOr("GANG_JWT_SECRET", ""),
		AccessTokenTTLSeconds:  envIntOr("GANG_ACCESS_TOKEN_TTL", 900),
		RefreshTokenTTLSeconds: envIntOr("GANG_REFRESH_TOKEN_TTL", 2592000),
		LoginMaxAttempts:       int(envIntOr("GANG_LOGIN_MAX_ATTEMPTS", 5)),
		LoginWindowSeconds:     envIntOr("GANG_LOGIN_WINDOW_SECONDS", 900),
		AssetDir:               envOr("GANG_ASSET_DIR", "assets"),
		StorageBackend:         envOr("GANG_STORAGE_BACKEND", ""),
		AssetPublicBaseURL:     envOr("GANG_ASSET_PUBLIC_BASE_URL", ""),
		AssetObjectPrefix:      envOr("GANG_ASSET_OBJECT_PREFIX", "assets"),
		AssetCacheControl:      envOr("GANG_ASSET_CACHE_CONTROL", "public, max-age=31536000, immutable"),
		AssetUploadMaxBytes:    envIntOr("GANG_ASSET_UPLOAD_MAX_BYTES", DefaultAssetUploadMaxBytes),
		ImageUploadMaxBytes:    envIntOr("GANG_IMAGE_UPLOAD_MAX_BYTES", DefaultImageUploadMaxBytes),
		COSBucket:              envOr("GANG_COS_BUCKET", ""),
		COSRegion:              envOr("GANG_COS_REGION", ""),
		COSBucketURL:           envOr("GANG_COS_BUCKET_URL", ""),
		COSSecretID:            envOr("GANG_COS_SECRET_ID", ""),
		COSSecretKey:           envOr("GANG_COS_SECRET_KEY", ""),
		COSSessionToken:        envOr("GANG_COS_SESSION_TOKEN", ""),
		COSObjectACL:           envOr("GANG_COS_OBJECT_ACL", "public-read"),
		GeoIPDatabasePath:      envOr("GANG_GEOIP_DB_PATH", ""),
		TrustedProxies:         envListOr("GANG_TRUSTED_PROXIES", []string{"127.0.0.1", "::1"}),
		AllowedOrigins:         envListOr("GANG_ALLOWED_ORIGINS", []string{"*"}),
		LiveKitHost:            envOr("LIVEKIT_HOST", "http://localhost:7880"),
		LiveKitAPIKey:          envOr("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret:       envOr("LIVEKIT_API_SECRET", ""),
		FFmpegPath:               envOr("GANG_FFMPEG_PATH", "ffmpeg"),
		MusicBoxDir:              envOr("GANG_MUSIC_BOX_DIR", "music-box"),
		MusicBoxMaxBytesPerRoom:  envIntOr("GANG_MUSIC_BOX_MAX_BYTES_PER_ROOM", DefaultMusicBoxMaxBytesPerRoom),
		MusicBoxOpusBitrate:      envOr("GANG_MUSIC_BOX_OPUS_BITRATE", "128k"),
		MusicBoxTranscodeWorkers: int(envIntOr("GANG_MUSIC_BOX_TRANSCODE_WORKERS", 3)),
		MusicBoxSource:           envOr("GANG_MUSIC_BOX_SOURCE", "netease"),
		MusicBoxSourceBitrate:    envOr("GANG_MUSIC_BOX_SOURCE_BITRATE", "192"),
	}

	trustedProxies := strings.Join(cfg.TrustedProxies, ",")
	flag.StringVar(&cfg.Bind, "bind", cfg.Bind, "listen address")
	flag.StringVar(&cfg.JWTSecret, "jwt-secret", cfg.JWTSecret, "JWT signing secret")
	flag.StringVar(&cfg.DatabaseURL, "database-url", cfg.DatabaseURL, "SQLite database path")
	flag.StringVar(&cfg.AssetDir, "asset-dir", cfg.AssetDir, "local asset cache directory")
	flag.StringVar(&cfg.StorageBackend, "storage-backend", cfg.StorageBackend, "asset storage backend: local or cos")
	flag.StringVar(&cfg.AssetPublicBaseURL, "asset-public-base-url", cfg.AssetPublicBaseURL, "optional CDN/COS public base URL for asset URLs")
	flag.StringVar(&cfg.AssetObjectPrefix, "asset-object-prefix", cfg.AssetObjectPrefix, "object storage prefix for uploaded assets")
	flag.StringVar(&cfg.AssetCacheControl, "asset-cache-control", cfg.AssetCacheControl, "Cache-Control header for uploaded assets")
	flag.Int64Var(&cfg.AssetUploadMaxBytes, "asset-upload-max-bytes", cfg.AssetUploadMaxBytes, "maximum uploaded file size in bytes")
	flag.Int64Var(&cfg.ImageUploadMaxBytes, "image-upload-max-bytes", cfg.ImageUploadMaxBytes, "maximum uploaded image size in bytes")
	flag.StringVar(&cfg.COSBucket, "cos-bucket", cfg.COSBucket, "Tencent COS bucket name, including appid")
	flag.StringVar(&cfg.COSRegion, "cos-region", cfg.COSRegion, "Tencent COS bucket region")
	flag.StringVar(&cfg.COSBucketURL, "cos-bucket-url", cfg.COSBucketURL, "Tencent COS bucket URL; overrides bucket and region")
	flag.StringVar(&cfg.COSSecretID, "cos-secret-id", cfg.COSSecretID, "Tencent COS secret id")
	flag.StringVar(&cfg.COSSecretKey, "cos-secret-key", cfg.COSSecretKey, "Tencent COS secret key")
	flag.StringVar(&cfg.COSSessionToken, "cos-session-token", cfg.COSSessionToken, "Tencent COS session token for temporary credentials")
	flag.StringVar(&cfg.COSObjectACL, "cos-object-acl", cfg.COSObjectACL, "COS object ACL for uploaded assets; empty keeps bucket default")
	flag.StringVar(&cfg.GeoIPDatabasePath, "geoip-db", cfg.GeoIPDatabasePath, "MaxMind GeoIP database path")
	flag.StringVar(&cfg.FFmpegPath, "ffmpeg-path", cfg.FFmpegPath, "path to the ffmpeg binary used for music transcoding")
	flag.StringVar(&cfg.MusicBoxDir, "music-box-dir", cfg.MusicBoxDir, "directory for transcoded room music files")
	flag.Int64Var(&cfg.MusicBoxMaxBytesPerRoom, "music-box-max-bytes-per-room", cfg.MusicBoxMaxBytesPerRoom, "max on-disk bytes of transcoded music per room")
	flag.StringVar(&cfg.MusicBoxOpusBitrate, "music-box-opus-bitrate", cfg.MusicBoxOpusBitrate, "Opus bitrate for broadcast transcode, e.g. 128k")
	flag.IntVar(&cfg.MusicBoxTranscodeWorkers, "music-box-transcode-workers", cfg.MusicBoxTranscodeWorkers, "max concurrent transcode jobs")
	flag.StringVar(&cfg.MusicBoxSource, "music-box-source", cfg.MusicBoxSource, "default GD music source")
	flag.StringVar(&cfg.MusicBoxSourceBitrate, "music-box-source-bitrate", cfg.MusicBoxSourceBitrate, "GD source download quality (128/192/320/740/999)")
	flag.StringVar(&trustedProxies, "trusted-proxies", trustedProxies, "comma-separated trusted proxy IPs/CIDRs")
	allowedOrigins := strings.Join(cfg.AllowedOrigins, ",")
	flag.StringVar(&allowedOrigins, "allowed-origins", allowedOrigins, "comma-separated allowed CORS origins, or * for any")
	flag.Parse()
	cfg.TrustedProxies = parseList(trustedProxies)
	cfg.AllowedOrigins = parseList(allowedOrigins)

	if cfg.JWTSecret == "" {
		panic("GANG_JWT_SECRET is required")
	}

	cfg.QQMusic = loadQQMusicConfig(envOr("GANG_QQMUSIC_CONFIG", "qqmusic.json"))

	return cfg
}

// QQMusicConfig holds the connection details for the self-hosted QQ音乐 API
// service. It's loaded from a JSON file (default qqmusic.json) so the password
// stays out of the environment/process listing and out of version control.
type QQMusicConfig struct {
	// BaseURL is the service origin, e.g. "http://103.47.83.189:12345".
	BaseURL string `json:"base_url"`
	// Password is the service access password (the config.json "password" on
	// the service side).
	Password string `json:"password"`
}

// loadQQMusicConfig reads the QQ音乐 service config from path. Behavior:
//   - file absent: returns nil (feature stays off; netease/bilibili unaffected).
//   - file present but unreadable/invalid/missing fields: panics, so a broken
//     config fails loudly at startup rather than silently disabling QQ音乐.
//
// Reachability and password correctness are verified separately at startup by
// calling the client's Login; that's where a wrong password or down service
// aborts the boot.
func loadQQMusicConfig(path string) *QQMusicConfig {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		panic("read QQ音乐 config " + path + ": " + err.Error())
	}
	var qc QQMusicConfig
	if err := json.Unmarshal(raw, &qc); err != nil {
		panic("parse QQ音乐 config " + path + ": " + err.Error())
	}
	qc.BaseURL = strings.TrimSpace(qc.BaseURL)
	qc.Password = strings.TrimSpace(qc.Password)
	if qc.BaseURL == "" || qc.Password == "" {
		panic("QQ音乐 config " + path + " must set both base_url and password")
	}
	return &qc
}
