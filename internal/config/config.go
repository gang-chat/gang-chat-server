package config

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
)

const (
	DefaultAssetUploadMaxBytes     int64 = 50 * 1024 * 1024
	DefaultImageUploadMaxBytes     int64 = 10 * 1024 * 1024
	DefaultMusicBoxMaxBytesPerRoom int64 = 200 * 1024 * 1024
)

type Config struct {
	Bind                     string   `json:"bind"`
	DatabaseURL              string   `json:"database_url"`
	JWTSecret                string   `json:"jwt_secret"`
	AccessTokenTTLSeconds    int64    `json:"access_token_ttl_seconds"`
	RefreshTokenTTLSeconds   int64    `json:"refresh_token_ttl_seconds"`
	LoginMaxAttempts         int      `json:"login_max_attempts"`
	LoginWindowSeconds       int64    `json:"login_window_seconds"`
	AssetUploadMaxBytes      int64    `json:"asset_upload_max_bytes"`
	ImageUploadMaxBytes      int64    `json:"image_upload_max_bytes"`
	S3Endpoint               string   `json:"s3_endpoint"`
	S3Bucket                 string   `json:"s3_bucket"`
	S3Region                 string   `json:"s3_region"`
	S3AccessKeyID            string   `json:"s3_access_key_id"`
	S3SecretAccessKey        string   `json:"s3_secret_access_key"`
	S3SessionToken           string   `json:"s3_session_token"`
	S3ForcePathStyle         bool     `json:"s3_force_path_style"`
	GeoIPDatabasePath        string   `json:"geoip_db_path"`
	TrustedProxies           []string `json:"trusted_proxies"`
	AllowedOrigins           []string `json:"allowed_origins"`
	LiveKitHost              string   `json:"livekit_host"`
	LiveKitAPIKey            string   `json:"livekit_api_key"`
	LiveKitAPISecret         string   `json:"livekit_api_secret"`
	FFmpegPath               string   `json:"ffmpeg_path"`
	MusicBoxDir              string   `json:"music_box_dir"`
	MusicBoxMaxBytesPerRoom  int64    `json:"music_box_max_bytes_per_room"`
	MusicBoxOpusBitrate      string   `json:"music_box_opus_bitrate"`
	MusicBoxTranscodeWorkers int      `json:"music_box_transcode_workers"`
	MusicBoxDownloadBitrate  string   `json:"music_box_download_bitrate"`

	QQMusicBaseURL  string `json:"qqmusic_base_url"`
	QQMusicPassword string `json:"qqmusic_password"`
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
	flag.StringVar(&cfg.DatabaseURL, "database-url", cfg.DatabaseURL, "MySQL DSN")
	flag.Int64Var(&cfg.AssetUploadMaxBytes, "asset-upload-max-bytes", cfg.AssetUploadMaxBytes, "maximum uploaded file size in bytes")
	flag.Int64Var(&cfg.ImageUploadMaxBytes, "image-upload-max-bytes", cfg.ImageUploadMaxBytes, "maximum uploaded image size in bytes")
	flag.StringVar(&cfg.S3Endpoint, "s3-endpoint", cfg.S3Endpoint, "S3-compatible endpoint URL")
	flag.StringVar(&cfg.S3Bucket, "s3-bucket", cfg.S3Bucket, "S3 bucket name")
	flag.StringVar(&cfg.S3Region, "s3-region", cfg.S3Region, "S3 signing region")
	flag.StringVar(&cfg.S3AccessKeyID, "s3-access-key-id", cfg.S3AccessKeyID, "S3 access key id")
	flag.StringVar(&cfg.S3SecretAccessKey, "s3-secret-access-key", cfg.S3SecretAccessKey, "S3 secret access key")
	flag.StringVar(&cfg.S3SessionToken, "s3-session-token", cfg.S3SessionToken, "S3 session token for temporary credentials")
	flag.BoolVar(&cfg.S3ForcePathStyle, "s3-force-path-style", cfg.S3ForcePathStyle, "use path-style S3 URLs")
	flag.StringVar(&cfg.GeoIPDatabasePath, "geoip-db", cfg.GeoIPDatabasePath, "MaxMind GeoIP database path")
	flag.StringVar(&cfg.FFmpegPath, "ffmpeg-path", cfg.FFmpegPath, "path to the ffmpeg binary used for music transcoding")
	flag.StringVar(&cfg.MusicBoxDir, "music-box-dir", cfg.MusicBoxDir, "directory for transcoded room music files")
	flag.Int64Var(&cfg.MusicBoxMaxBytesPerRoom, "music-box-max-bytes-per-room", cfg.MusicBoxMaxBytesPerRoom, "max on-disk bytes of transcoded music per room")
	flag.StringVar(&cfg.MusicBoxOpusBitrate, "music-box-opus-bitrate", cfg.MusicBoxOpusBitrate, "Opus bitrate for broadcast transcode, e.g. 128k")
	flag.IntVar(&cfg.MusicBoxTranscodeWorkers, "music-box-transcode-workers", cfg.MusicBoxTranscodeWorkers, "max concurrent transcode jobs")
	flag.StringVar(&cfg.MusicBoxDownloadBitrate, "music-box-download-bitrate", cfg.MusicBoxDownloadBitrate, "GD download quality (128/192/320/740/999)")
	flag.StringVar(&trustedProxies, "trusted-proxies", trustedProxies, "comma-separated trusted proxy IPs/CIDRs")
	allowedOrigins := strings.Join(cfg.AllowedOrigins, ",")
	flag.StringVar(&allowedOrigins, "allowed-origins", allowedOrigins, "comma-separated allowed CORS origins, or * for any")
	flag.Parse()
	cfg.TrustedProxies = parseList(trustedProxies)
	cfg.AllowedOrigins = parseList(allowedOrigins)

	if cfg.JWTSecret == "" {
		panic("jwt_secret is required in config.json")
	}

	return &cfg
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
