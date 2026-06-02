package config

import (
	"flag"
	"os"
	"strconv"
	"strings"
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
	GeoIPDatabasePath      string
	TrustedProxies         []string
	LiveKitHost            string
	LiveKitAPIKey          string
	LiveKitAPISecret       string
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
		GeoIPDatabasePath:      envOr("GANG_GEOIP_DB_PATH", ""),
		TrustedProxies:         envListOr("GANG_TRUSTED_PROXIES", []string{"127.0.0.1", "::1"}),
		LiveKitHost:            envOr("LIVEKIT_HOST", "http://localhost:7880"),
		LiveKitAPIKey:          envOr("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret:       envOr("LIVEKIT_API_SECRET", ""),
	}

	trustedProxies := strings.Join(cfg.TrustedProxies, ",")
	flag.StringVar(&cfg.Bind, "bind", cfg.Bind, "listen address")
	flag.StringVar(&cfg.JWTSecret, "jwt-secret", cfg.JWTSecret, "JWT signing secret")
	flag.StringVar(&cfg.DatabaseURL, "database-url", cfg.DatabaseURL, "SQLite database path")
	flag.StringVar(&cfg.AssetDir, "asset-dir", cfg.AssetDir, "uploaded asset directory")
	flag.StringVar(&cfg.GeoIPDatabasePath, "geoip-db", cfg.GeoIPDatabasePath, "MaxMind GeoIP database path")
	flag.StringVar(&trustedProxies, "trusted-proxies", trustedProxies, "comma-separated trusted proxy IPs/CIDRs")
	flag.Parse()
	cfg.TrustedProxies = parseList(trustedProxies)

	if cfg.JWTSecret == "" {
		panic("GANG_JWT_SECRET is required")
	}

	return cfg
}
