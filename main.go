package main

import (
	"context"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	"github.com/zhuangkaiyi/gang-chat/server/internal/chat"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	livekithandler "github.com/zhuangkaiyi/gang-chat/server/internal/livekit"
	"github.com/zhuangkaiyi/gang-chat/server/internal/livekitwebhook"
	"github.com/zhuangkaiyi/gang-chat/server/internal/middleware"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
	"github.com/zhuangkaiyi/gang-chat/server/internal/qqmusic"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

func main() {
	_ = godotenv.Load()
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

	// QQ音乐 integration is optional: enabled only when a qqmusic.json config is
	// present. When present, a failed login (service down or wrong password)
	// aborts startup, so a misconfiguration is loud rather than silently off.
	var qqClient *qqmusic.Client
	if cfg.QQMusic != nil {
		qc, err := qqmusic.New(cfg.QQMusic.BaseURL, cfg.QQMusic.Password)
		if err != nil {
			log.Fatalf("qqmusic: init client: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := qc.Login(ctx); err != nil {
			cancel()
			log.Fatalf("qqmusic: startup health check failed: %v", err)
		}
		cancel()
		qqClient = qc
		log.Printf("qqmusic: connected to %s", cfg.QQMusic.BaseURL)
	}

	// Music box: server-side download/transcode/broadcast of room music. It
	// needs LiveKit to publish a bot track, so it's only enabled when LiveKit
	// is configured. The token func issues a publish-only token for the bot.
	musicBox := musicbox.NewManager(pool, musicbox.Config{
		Dir:              cfg.MusicBoxDir,
		MaxBytesPerRoom:  cfg.MusicBoxMaxBytesPerRoom,
		FFmpegPath:       cfg.FFmpegPath,
		OpusBitrate:      cfg.MusicBoxOpusBitrate,
		TranscodeWorkers: cfg.MusicBoxTranscodeWorkers,
		Source:           cfg.MusicBoxSource,
		SourceBitrate:    cfg.MusicBoxSourceBitrate,
		LiveKitHost:      cfg.LiveKitHost,
		Enabled:          cfg.LiveKitAPIKey != "" && cfg.LiveKitAPISecret != "",
		QQ:               qqClient,
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

	r := gin.Default()
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Fatalf("configure trusted proxies: %v", err)
	}
	r.Use(middleware.RequestID())
	r.Use(middleware.CORS(cfg.AllowedOrigins...))

	r.GET("/health", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	chat.RegisterAssetRoutes(r, pool, cfg, assetStore)

	api := r.Group("/api/v1")
	auth.RegisterRoutes(api, pool, cfg)

	authMW := &auth.AuthMiddleware{DB: pool, JWTSecret: cfg.JWTSecret}
	chatGroup := api.Group("")
	chatGroup.Use(authMW.Handle)
	chatHandler := chat.RegisterRoutes(chatGroup, pool, cfg, bus, liveController, musicBox, assetStore)

	lkGroup := r.Group("/livekit")
	lkGroup.Use(authMW.Handle)
	livekithandler.RegisterRoutes(lkGroup, roomClient, cfg)

	// LiveKit webhooks authenticate via their own signed token, not our JWT,
	// so they must NOT go through authMW.
	webhookGroup := r.Group("/webhooks")
	livekitwebhook.RegisterRoutes(webhookGroup, &livekitwebhook.Handler{
		DB:          pool,
		Cfg:         cfg,
		Bus:         bus,
		PublishLive: chatHandler.PublishLiveSnapshot,
	})

	log.Printf("api server listening on %s", cfg.Bind)
	if err := r.Run(cfg.Bind); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
