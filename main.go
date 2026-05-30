package main

import (
	"log"

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

	lksdk "github.com/livekit/server-sdk-go/v2"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	pool := db.Connect(cfg.DatabaseURL)

	roomClient := lksdk.NewRoomServiceClient(
		cfg.LiveKitHost,
		cfg.LiveKitAPIKey,
		cfg.LiveKitAPISecret,
	)

	bus := eventbus.New()

	r := gin.Default()
	r.Use(middleware.RequestID())
	r.Use(middleware.CORS())

	r.GET("/health", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	api := r.Group("/api/v1")
	auth.RegisterRoutes(api, pool, cfg)

	authMW := &auth.AuthMiddleware{DB: pool, JWTSecret: cfg.JWTSecret}
	chatGroup := api.Group("")
	chatGroup.Use(authMW.Handle)
	chatHandler := chat.RegisterRoutes(chatGroup, pool, cfg, bus)

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
