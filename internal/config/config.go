package config

import (
	"flag"
	"os"
	"strconv"
)

type Config struct {
	Bind                   string
	DatabaseURL            string
	JWTSecret              string
	AccessTokenTTLSeconds  int64
	RefreshTokenTTLSeconds int64
	LoginMaxAttempts       int
	LoginWindowSeconds     int64
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

func Load() *Config {
	cfg := &Config{
		Bind:                   envOr("GANG_BIND", "127.0.0.1:21116"),
		DatabaseURL:            envOr("GANG_DATABASE_URL", "gang-chat.db"),
		JWTSecret:              envOr("GANG_JWT_SECRET", ""),
		AccessTokenTTLSeconds:  envIntOr("GANG_ACCESS_TOKEN_TTL", 900),
		RefreshTokenTTLSeconds: envIntOr("GANG_REFRESH_TOKEN_TTL", 2592000),
		LoginMaxAttempts:       int(envIntOr("GANG_LOGIN_MAX_ATTEMPTS", 5)),
		LoginWindowSeconds:     envIntOr("GANG_LOGIN_WINDOW_SECONDS", 900),
		LiveKitHost:            envOr("LIVEKIT_HOST", "http://localhost:7880"),
		LiveKitAPIKey:          envOr("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret:       envOr("LIVEKIT_API_SECRET", ""),
	}

	flag.StringVar(&cfg.Bind, "bind", cfg.Bind, "listen address")
	flag.StringVar(&cfg.JWTSecret, "jwt-secret", cfg.JWTSecret, "JWT signing secret")
	flag.StringVar(&cfg.DatabaseURL, "database-url", cfg.DatabaseURL, "SQLite database path")
	flag.Parse()

	if cfg.JWTSecret == "" {
		panic("GANG_JWT_SECRET is required")
	}

	return cfg
}
