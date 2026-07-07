package chat

import (
	"database/sql"
	"log"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

// liveMediaController is the subset of LiveKit room-control operations the
// chat layer drives. *livekit.Controller satisfies it; tests inject a fake.
// When LiveKit isn't configured (dev mode / tests) RegisterRoutes substitutes
// noopLiveController so call sites never hold a nil interface and moderation
// degrades cleanly to DB-only bookkeeping.
type liveMediaController interface {
	RemoveParticipant(room, identity string) error
	SetMediaPermissions(room, identity string, canPublish, canSubscribe bool) error
	SetCanPublish(room, identity string, canPublish bool) error
	SetCanSubscribe(room, identity string, canSubscribe bool) error
	MuteMicrophone(room, identity string, muted bool) error
}

// noopLiveController is the DB-only fallback: every media-control op succeeds
// without touching a LiveKit session.
type noopLiveController struct{}

func (noopLiveController) RemoveParticipant(string, string) error               { return nil }
func (noopLiveController) SetMediaPermissions(string, string, bool, bool) error { return nil }
func (noopLiveController) SetCanPublish(string, string, bool) error             { return nil }
func (noopLiveController) SetCanSubscribe(string, string, bool) error           { return nil }
func (noopLiveController) MuteMicrophone(string, string, bool) error            { return nil }

type Handler struct {
	DB       *sql.DB
	Cfg      *config.Config
	Bus      *eventbus.Bus
	Live     liveMediaController
	Assets   *storage.AssetStorage
	MusicBox *musicbox.Manager

	stickerPackLocks keyedMutexes
}

// RegisterRoutes wires the chat API onto g and returns the handler so the
// caller (main.go) can reuse it elsewhere — e.g. to let the LiveKit webhook
// publish live snapshots through the same bus. bus may be nil (tests); all
// publish paths tolerate a nil bus. live may be nil; moderation then degrades
// to DB-only bookkeeping without driving the LiveKit media session.
func RegisterRoutes(g *gin.RouterGroup, db *sql.DB, cfg *config.Config, bus *eventbus.Bus, live liveMediaController, mb *musicbox.Manager, assetStores ...*storage.AssetStorage) *Handler {
	if live == nil {
		live = noopLiveController{}
	}
	assetStore := firstAssetStore(assetStores)
	if assetStore == nil {
		var err error
		assetStore, err = storage.NewAssetStorage(cfg)
		if err != nil {
			panic(err)
		}
	}
	h := &Handler{DB: db, Cfg: cfg, Bus: bus, Live: live, Assets: assetStore, MusicBox: mb}
	if err := h.ensureRoomNotificationSchema(); err != nil {
		log.Printf("chat: ensure room notification schema: %v", err)
	}

	// The music box fans out a fresh snapshot whenever a room's queue or
	// playback changes; route it through the same SSE bus as everything else.
	if mb != nil {
		mb.SetOnRoomChanged(func(roomID string) {
			h.publishMusicBoxSnapshot(roomID)
		})
	}

	g.GET("/me/stream", h.liveStream)
	g.GET("/app/version", h.getAppVersion)

	g.POST("/uploads/images", h.uploadFile)
	g.POST("/uploads/files", h.uploadFile)
	g.GET("/sticker-packs", h.listStickerPacks)
	g.POST("/sticker-packs", h.createStickerPack)
	g.PATCH("/sticker-packs/:pack_id", h.updateStickerPack)
	g.DELETE("/sticker-packs/:pack_id", h.deleteStickerPack)
	g.POST("/sticker-packs/:pack_id/stickers", h.addSticker)
	g.POST("/sticker-packs/:pack_id/stickers/reorder", h.reorderStickers)
	g.PATCH("/sticker-packs/:pack_id/stickers/:sticker_id", h.updateSticker)
	g.DELETE("/sticker-packs/:pack_id/stickers/:sticker_id", h.deleteSticker)
	g.GET("/stickers/download", h.downloadStickers)
	g.POST("/rooms/:room_id/stickers/save", h.saveSticker)

	g.GET("/rooms", h.listRooms)
	g.POST("/rooms", h.createRoom)
	g.GET("/search", h.searchAll)
	g.GET("/rooms/search", h.searchRooms)
	g.GET("/users/:user_id/profile", h.getUserProfile)
	g.GET("/room-invites", h.listRoomInvites)
	g.PATCH("/room-invites/:invite_id", h.reviewRoomInvite)
	g.GET("/room-applications", h.listRoomApplications)
	g.PATCH("/room-applications/:request_id", h.withdrawRoomApplication)
	g.GET("/room-notifications", h.listRoomNotifications)
	g.POST("/room-notifications/read", h.markRoomNotificationsRead)
	g.GET("/rooms/:room_id", h.getRoom)
	g.PATCH("/rooms/:room_id", h.updateRoomSettings)
	g.GET("/rooms/:room_id/members", h.listMembers)
	g.GET("/rooms/:room_id/members/:user_id/profile", h.getMemberProfile)
	g.POST("/rooms/:room_id/join", h.joinRoom)
	g.POST("/rooms/:room_id/leave", h.leaveRoom)
	g.DELETE("/rooms/:room_id", h.deleteRoom)
	g.GET("/rooms/:room_id/me/settings", h.getMyRoomSettings)
	// Alias of PATCH /rooms/:room_id/me below; both update the caller's own
	// per-room settings. Kept for client compatibility.
	g.PATCH("/rooms/:room_id/me/settings", h.updateMyRoomSettings)
	g.PATCH("/rooms/:room_id/me", h.updateMyRoomSettings)
	g.GET("/rooms/:room_id/settings", h.getRoomSettings)
	// Alias of PATCH /rooms/:room_id above (room-level settings).
	g.PATCH("/rooms/:room_id/settings", h.updateRoomSettings)
	g.POST("/rooms/:room_id/invites", h.inviteMember)
	g.GET("/rooms/:room_id/blacklist", h.listRoomBlacklist)
	g.POST("/rooms/:room_id/blacklist", h.blockRoomUser)
	g.DELETE("/rooms/:room_id/blacklist/:user_id", h.unblockRoomUser)
	g.DELETE("/rooms/:room_id/members/:user_id", h.removeMember)
	g.PATCH("/rooms/:room_id/members/:user_id", h.updateMemberRole)
	// Alias of PATCH /rooms/:room_id/members/:user_id (role change). Kept for
	// client compatibility.
	g.PATCH("/rooms/:room_id/members/:user_id/role", h.updateMemberRole)
	g.PATCH("/rooms/:room_id/creator", h.transferRoomCreator)
	g.POST("/rooms/:room_id/members/:user_id/text-mute", h.textMuteMember)
	g.POST("/rooms/:room_id/members/:user_id/text-unmute", h.textUnmuteMember)
	g.GET("/rooms/:room_id/join-requests", h.listJoinRequests)
	g.PATCH("/rooms/:room_id/join-requests/:request_id", h.reviewJoinRequest)

	g.GET("/rooms/:room_id/messages", h.listMessages)
	g.POST("/rooms/:room_id/messages", h.sendMessage)
	g.POST("/rooms/:room_id/messages/:message_id/recall", h.recallMessage)
	g.GET("/rooms/:room_id/message-recall-requests", h.listMessageRecallRequests)
	g.PATCH("/rooms/:room_id/message-recall-requests/:request_id", h.reviewMessageRecallRequest)
	g.POST("/rooms/:room_id/messages/:message_id/force-delete", h.forceDeleteMessage)
	g.POST("/rooms/:room_id/read", h.markRead)

	g.GET("/rooms/:room_id/live", h.getLiveState)
	g.POST("/rooms/:room_id/live/join", h.joinLive)
	g.PATCH("/rooms/:room_id/live/me", h.updateMyLiveState)
	g.GET("/rooms/:room_id/live/me/member-volumes", h.listMyLiveMemberVolumes)
	g.PATCH("/rooms/:room_id/live/me/member-volumes/:target_user_id", h.updateMyLiveMemberVolume)
	g.POST("/rooms/:room_id/live/participants/:user_id/moderation", h.moderateLiveParticipant)
	g.POST("/rooms/:room_id/live/screen-audio-token", h.issueScreenAudioToken)

	// Server-side music box: search/enqueue/control a broadcast track.
	g.GET("/rooms/:room_id/music-box/search", h.searchMusicBox)
	g.GET("/rooms/:room_id/music-box/state", h.getMusicBoxState)
	g.POST("/rooms/:room_id/music-box/queue", h.enqueueMusicBox)
	g.DELETE("/rooms/:room_id/music-box/queue/:item_id", h.removeMusicBoxItem)
	g.POST("/rooms/:room_id/music-box/control", h.controlMusicBox)

	return h
}

func firstAssetStore(stores []*storage.AssetStorage) *storage.AssetStorage {
	for _, store := range stores {
		if store != nil {
			return store
		}
	}
	return nil
}
