package main

import (
	"context"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	"github.com/zhuangkaiyi/gang-chat/server/internal/chat"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	livekithandler "github.com/zhuangkaiyi/gang-chat/server/internal/livekit"
	"github.com/zhuangkaiyi/gang-chat/server/internal/livekitwebhook"
	"github.com/zhuangkaiyi/gang-chat/server/internal/middleware"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
	"github.com/zhuangkaiyi/gang-chat/server/internal/push"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

func main() {
	cfg := config.Load()

	pool := db.Connect(cfg.DatabaseURL)
	assetStore, err := storage.NewAssetStorage(cfg)
	if err != nil {
		log.Fatalf("configure asset storage: %v", err)
	}

	roomClient := lksdk.NewRoomServiceClient(
		cfg.LiveKitHost,
		cfg.LiveKitAPIKey,
		cfg.LiveKitAPISecret,
	)
	// liveController drives the LiveKit media session for moderation (kick /
	// mute / voice-block). Without API credentials there's no usable control
	// plane, so leave it nil and let the chat layer degrade to DB-only state.
	var liveController *livekithandler.Controller
	if cfg.LiveKitAPIKey != "" && cfg.LiveKitAPISecret != "" {
		liveController = livekithandler.NewController(roomClient)
	}

	bus := eventbus.New()

	// QQ音乐 is an optional source. Its availability must never prevent the
	// chat API from starting; when its startup probe fails, the manager receives
	// a nil client and exposes the remaining music sources normally.
	qqClient := initOptionalQQMusic(
		cfg.QQMusicBaseURL,
		cfg.QQMusicPassword,
		defaultQQMusicStartupTimeout,
		log.Printf,
	)

	// Music box: server-side download/transcode/broadcast of room music. It
	// needs LiveKit to publish a bot track, so it's only enabled when LiveKit
	// is configured. The token func issues a publish-only token for the bot.
	musicBox := musicbox.NewManager(pool, musicbox.Config{
		Dir:                  cfg.MusicBoxDir,
		MaxBytesPerRoom:      cfg.MusicBoxMaxBytesPerRoom,
		CacheMaxBytes:        cfg.MusicBoxCacheMaxBytes,
		FFmpegPath:           cfg.FFmpegPath,
		OpusBitrate:          cfg.MusicBoxOpusBitrate,
		TranscodeWorkers:     cfg.MusicBoxTranscodeWorkers,
		DownloadBitrate:      cfg.MusicBoxDownloadBitrate,
		CompactProgressOnly:  cfg.MusicBoxCompactProgressOnly,
		EmptyRoomGracePeriod: time.Duration(cfg.MusicBoxEmptyRoomGraceSeconds) * time.Second,
		LiveKitHost:          cfg.LiveKitHost,
		Enabled:              cfg.LiveKitAPIKey != "" && cfg.LiveKitAPISecret != "",
		QQ:                   qqClient,
	}, func(roomID, identity string) (string, error) {
		return livekithandler.GenerateJoinToken(livekithandler.TokenParams{
			APIKey:       cfg.LiveKitAPIKey,
			APISecret:    cfg.LiveKitAPISecret,
			Room:         roomID,
			Identity:     identity,
			Name:         "Music Box",
			CanPublish:   true,
			CanSubscribe: false,
		})
	}, nil)
	go musicBox.RunObservabilityLog(context.Background(), 5*time.Minute)

	r := gin.Default()
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Fatalf("configure trusted proxies: %v", err)
	}
	r.Use(middleware.RequestID())
	r.Use(middleware.ServerTime())
	r.Use(middleware.CORS(cfg.AllowedOrigins...))

	r.GET("/health", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	chat.RegisterAssetRoutes(r, pool, cfg, assetStore)

	api := r.Group("/api/v1")
	auth.RegisterRoutes(api, pool, cfg, bus)

	authMW := &auth.AuthMiddleware{DB: pool, JWTSecret: cfg.JWTSecret}
	chatGroup := api.Group("")
	chatGroup.Use(authMW.Handle)
	chatHandler := chat.RegisterRoutes(chatGroup, pool, cfg, bus, liveController, musicBox, assetStore)
	pushHandler := push.RegisterRoutes(chatGroup, pool)
	if err := pushHandler.Store.EnsureSchema(); err != nil {
		log.Fatalf("push: ensure device schema: %v", err)
	}
	fcmSender, err := push.NewFCMSender(cfg.FCMProjectID, cfg.FCMServiceAccountFile)
	if err != nil {
		log.Fatalf("push: configure FCM: %v", err)
	}
	pushDispatcher := push.NewDispatcher(pool, fcmSender)
	chatHandler.Push = pushDispatcher
	go pushDispatcher.Run(context.Background())
	go chatHandler.RunStickerAssetCleanup(context.Background())

	lkGroup := r.Group("/livekit")
	lkGroup.Use(authMW.Handle)
	livekithandler.RegisterRoutes(lkGroup, roomClient, cfg)

	// LiveKit webhooks authenticate via their own signed token, not our JWT,
	// so they must NOT go through authMW.
	webhookGroup := r.Group("/webhooks")
	livekitWebhookHandler := &livekitwebhook.Handler{
		DB:          pool,
		Cfg:         cfg,
		Bus:         bus,
		RoomClient:  roomClient,
		PublishLive: chatHandler.PublishLiveSnapshot,
	}
	livekitwebhook.RegisterRoutes(webhookGroup, livekitWebhookHandler)
	go livekitWebhookHandler.Run(context.Background())

	log.Printf("api server listening on %s", cfg.Bind)
	if err := r.Run(cfg.Bind); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
