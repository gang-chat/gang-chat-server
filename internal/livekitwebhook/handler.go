// Package livekitwebhook receives LiveKit server webhook callbacks and turns
// them into eventbus publishes. This is the fallback path for cases where the
// client never gets a chance to tell us it left (process killed, network
// dropped): LiveKit detects the disconnect via RTCP timeout and fires
// participant_left / room_finished, which we use to clean up live_participants
// and notify the room's SSE subscribers.
package livekitwebhook

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
)

type Handler struct {
	DB  *sql.DB
	Cfg *config.Config
	Bus *eventbus.Bus
	// PublishLive lets the webhook reuse the chat handler's snapshot builder
	// (buildLiveState / livePreview) without an import cycle. Injected from
	// main.go as chatHandler.PublishLiveSnapshot.
	PublishLive func(roomID, eventType string, extra map[string]any)
}

func RegisterRoutes(g *gin.RouterGroup, h *Handler) {
	g.POST("/livekit", h.receive)
}

func (h *Handler) receive(c *gin.Context) {
	if h.Cfg.LiveKitAPIKey == "" || h.Cfg.LiveKitAPISecret == "" {
		// Dev mode (no LiveKit keys): webhooks can't be verified and won't be
		// sent anyway. Accept-and-ignore so a misconfigured LiveKit doesn't
		// spam errors.
		c.Status(http.StatusOK)
		return
	}

	provider := auth.NewSimpleKeyProvider(h.Cfg.LiveKitAPIKey, h.Cfg.LiveKitAPISecret)
	ev, err := webhook.ReceiveWebhookEvent(c.Request, provider)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	switch ev.GetEvent() {
	case "participant_left":
		roomName := ev.GetRoom().GetName()
		identity := ev.GetParticipant().GetIdentity()
		if roomName == "" || identity == "" {
			break
		}
		// Business room id == LiveKit room name (live_core.go issues tokens
		// with Room = roomID), and participant identity == user_id.
		res, err := h.DB.Exec(
			`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`,
			roomName, identity,
		)
		if err != nil {
			log.Printf("livekit webhook: delete participant failed: %v", err)
			break
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			h.publish(roomName, "live_participant_left", map[string]any{
				"user_id": identity,
				"reason":  "livekit_disconnected",
			})
		}
	case "room_finished":
		roomName := ev.GetRoom().GetName()
		if roomName == "" {
			break
		}
		if _, err := h.DB.Exec(`DELETE FROM live_participants WHERE room_id = ?`, roomName); err != nil {
			log.Printf("livekit webhook: clear room failed: %v", err)
			break
		}
		h.publish(roomName, "live_room_finished", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handler) publish(roomID, eventType string, extra map[string]any) {
	if h.PublishLive == nil {
		return
	}
	h.PublishLive(roomID, eventType, extra)
}
