package chat

import (
	"database/sql"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
)

type Handler struct {
	DB  *sql.DB
	Cfg *config.Config
	Bus *eventbus.Bus
}

// RegisterRoutes wires the chat API onto g and returns the handler so the
// caller (main.go) can reuse it elsewhere — e.g. to let the LiveKit webhook
// publish live snapshots through the same bus. bus may be nil (tests); all
// publish paths tolerate a nil bus.
func RegisterRoutes(g *gin.RouterGroup, db *sql.DB, cfg *config.Config, bus *eventbus.Bus) *Handler {
	h := &Handler{DB: db, Cfg: cfg, Bus: bus}

	g.GET("/me/stream", h.liveStream)

	g.POST("/uploads/images", h.uploadFile)
	g.POST("/uploads/files", h.uploadFile)
	g.GET("/sticker-packs", h.listStickerPacks)
	g.POST("/sticker-packs", h.createStickerPack)
	g.PATCH("/sticker-packs/:pack_id", h.updateStickerPack)
	g.DELETE("/sticker-packs/:pack_id", h.deleteStickerPack)
	g.POST("/sticker-packs/:pack_id/stickers", h.addSticker)
	g.DELETE("/sticker-packs/:pack_id/stickers/:sticker_id", h.deleteSticker)
	g.POST("/rooms/:room_id/stickers/save", h.saveSticker)

	g.GET("/rooms", h.listRooms)
	g.POST("/rooms", h.createRoom)
	g.GET("/rooms/search", h.searchRooms)
	g.GET("/rooms/:room_id", h.getRoom)
	g.GET("/rooms/:room_id/members", h.listMembers)
	g.GET("/rooms/:room_id/members/:user_id/profile", h.getMemberProfile)
	g.POST("/rooms/:room_id/join", h.joinRoom)
	g.POST("/rooms/:room_id/leave", h.leaveRoom)
	g.DELETE("/rooms/:room_id", h.deleteRoom)
	g.GET("/rooms/:room_id/me/settings", h.getMyRoomSettings)
	g.PATCH("/rooms/:room_id/me/settings", h.updateMyRoomSettings)
	g.GET("/rooms/:room_id/settings", h.getRoomSettings)
	g.PATCH("/rooms/:room_id/settings", h.updateRoomSettings)
	g.POST("/rooms/:room_id/invites", h.inviteMember)
	g.DELETE("/rooms/:room_id/members/:user_id", h.removeMember)
	g.PATCH("/rooms/:room_id/members/:user_id/role", h.updateMemberRole)
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
	g.GET("/rooms/:room_id/music/state", h.getMusicState)
	g.POST("/rooms/:room_id/music/playback", h.controlMusicPlayback)
	g.POST("/rooms/:room_id/music/session", h.controlMusicSession)
	g.POST("/rooms/:room_id/music/invites", h.createMusicInvites)
	g.POST("/rooms/:room_id/music/queue", h.addMusicQueue)
	g.GET("/rooms/:room_id/playlists", h.listRoomPlaylists)
	g.POST("/rooms/:room_id/playlists", h.createRoomPlaylist)
	g.PATCH("/rooms/:room_id/playlists/:playlist_id", h.updateRoomPlaylist)
	g.DELETE("/rooms/:room_id/playlists/:playlist_id", h.deleteRoomPlaylist)
	g.POST("/rooms/:room_id/playlists/:playlist_id/tracks", h.addRoomPlaylistTrack)
	g.PATCH("/rooms/:room_id/playlists/:playlist_id/tracks/:track_id", h.updateRoomPlaylistTrack)
	g.DELETE("/rooms/:room_id/playlists/:playlist_id/tracks/:track_id", h.deleteRoomPlaylistTrack)

	return h
}
