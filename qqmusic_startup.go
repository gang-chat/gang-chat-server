package main

import (
	"context"
	"strings"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/qqmusic"
)

const defaultQQMusicStartupTimeout = 5 * time.Second

type logPrintf func(format string, args ...any)

// initOptionalQQMusic probes the optional QQ音乐 service once during startup.
// A failed probe disables only that music source; the chat server remains
// available. Recovery is picked up on the next normal service restart.
func initOptionalQQMusic(
	baseURL string,
	password string,
	timeout time.Duration,
	logf logPrintf,
) *qqmusic.Client {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(password) == "" {
		return nil
	}

	client, err := qqmusic.New(baseURL, password)
	if err != nil {
		logf("qqmusic: disabled because client setup failed: %v", err)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := client.Login(ctx); err != nil {
		logf("qqmusic: unavailable at startup; source disabled: %v", err)
		return nil
	}

	logf("qqmusic: connected to %s", strings.TrimRight(baseURL, "/"))
	return client
}
