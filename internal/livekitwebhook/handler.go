// Package livekitwebhook receives LiveKit server webhook callbacks and turns
// them into eventbus publishes. This is the fallback path for cases where the
// client never gets a chance to tell us it left (process killed, network
// dropped): LiveKit detects the disconnect via RTCP timeout and fires
// participant_left / room_finished, which we use to clean up live_participants
// and notify the room's SSE subscribers.
package livekitwebhook

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	livekittoken "github.com/zhuangkaiyi/gang-chat/server/internal/livekit"
)

type Handler struct {
	DB         *sql.DB
	Cfg        *config.Config
	Bus        *eventbus.Bus
	RoomClient *lksdk.RoomServiceClient
	// PublishLive lets the webhook reuse the chat handler's snapshot builder
	// (buildLiveState / livePreview) without an import cycle. Injected from
	// main.go as chatHandler.PublishLiveSnapshot.
	PublishLive func(roomID, eventType string, extra map[string]any)
}

const (
	liveReconnectGracePeriod = 30 * time.Second
	liveJoiningGracePeriod   = 2 * time.Minute
	liveReconnectSweepPeriod = 5 * time.Second
)

const expiredLiveTransitionsQuery = `SELECT room_id, user_id, client_live_session_id, connection_state
		   FROM live_participants
		  WHERE (connection_state = 'reconnecting' AND updated_at <= ?)
		     OR (connection_state = 'joining' AND updated_at <= ?)`

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
		log.Printf("livekit webhook: verification failed: %v", err)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "回调验证失败"})
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
		query, args := participantLeftReconnectUpdate(
			roomName,
			identity,
			ev.GetParticipant().GetMetadata(),
			ev.GetParticipant().GetJoinedAtMs(),
			time.Now().UnixMilli(),
		)
		res, err := h.DB.Exec(query, args...)
		if err != nil {
			log.Printf("livekit webhook: mark participant reconnecting failed: %v", err)
			break
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			h.publish(roomName, "live_participant_reconnecting", map[string]any{
				"user_id": identity,
				"reason":  "livekit_disconnected",
			})
		}
	case "participant_joined":
		roomName := ev.GetRoom().GetName()
		identity := ev.GetParticipant().GetIdentity()
		if roomName == "" || identity == "" {
			break
		}
		query, args := participantRejoinedUpdate(
			roomName,
			identity,
			ev.GetParticipant().GetMetadata(),
			ev.GetParticipant().GetJoinedAtMs(),
			time.Now().UnixMilli(),
		)
		res, err := h.DB.Exec(query, args...)
		if err != nil {
			log.Printf("livekit webhook: restore reconnected participant failed: %v", err)
			break
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			h.publish(roomName, "live_participant_reconnected", map[string]any{
				"user_id": identity,
			})
		}
	case "room_finished":
		roomName := ev.GetRoom().GetName()
		if roomName == "" {
			break
		}
		res, err := h.DB.Exec(
			`UPDATE live_participants
			    SET connection_state = 'reconnecting', updated_at = ?
			  WHERE room_id = ? AND connection_state != 'left'`,
			time.Now().UnixMilli(),
			roomName,
		)
		if err != nil {
			log.Printf("livekit webhook: mark finished room reconnecting failed: %v", err)
			break
		}
		_, _ = h.DB.Exec(
			`DELETE FROM live_participants WHERE room_id = ? AND connection_state = 'left'`,
			roomName,
		)
		if affected, _ := res.RowsAffected(); affected > 0 {
			h.publish(roomName, "live_room_reconnecting", nil)
		} else {
			h.publish(roomName, "live_room_finished", nil)
		}
	}
	c.Status(http.StatusOK)
}

func participantLeftReconnectUpdate(
	roomName,
	identity,
	metadata string,
	joinedAtMillis int64,
	updatedAtMillis int64,
) (string, []any) {
	if clientLiveSessionID, ok :=
		livekittoken.ClientLiveSessionIDFromMetadata(metadata); ok {
		return `UPDATE live_participants
		           SET connection_state = 'reconnecting', updated_at = ?
		         WHERE room_id = ? AND user_id = ? AND client_live_session_id = ?
		           AND connection_state != 'left'`,
			[]any{updatedAtMillis, roomName, identity, clientLiveSessionID}
	}
	if joinedAtMillis > 0 {
		return `UPDATE live_participants
		           SET connection_state = 'reconnecting', updated_at = ?
		         WHERE room_id = ? AND user_id = ? AND joined_at <= ?
		           AND connection_state != 'left'`,
			[]any{updatedAtMillis, roomName, identity, joinedAtMillis}
	}
	return `UPDATE live_participants
	           SET connection_state = 'reconnecting', updated_at = ?
	         WHERE room_id = ? AND user_id = ? AND connection_state != 'left'`,
		[]any{updatedAtMillis, roomName, identity}
}

func participantRejoinedUpdate(
	roomName,
	identity,
	metadata string,
	joinedAtMillis int64,
	updatedAtMillis int64,
) (string, []any) {
	if clientLiveSessionID, ok :=
		livekittoken.ClientLiveSessionIDFromMetadata(metadata); ok {
		return `UPDATE live_participants
		           SET connection_state = 'online', updated_at = ?
		         WHERE room_id = ? AND user_id = ? AND client_live_session_id = ?
		           AND connection_state IN ('joining', 'reconnecting')`,
			[]any{updatedAtMillis, roomName, identity, clientLiveSessionID}
	}
	if joinedAtMillis > 0 {
		return `UPDATE live_participants
		           SET connection_state = 'online', updated_at = ?
		         WHERE room_id = ? AND user_id = ? AND joined_at <= ?
		           AND connection_state IN ('joining', 'reconnecting')`,
			[]any{updatedAtMillis, roomName, identity, joinedAtMillis}
	}
	return `UPDATE live_participants
	           SET connection_state = 'online', updated_at = ?
	         WHERE room_id = ? AND user_id = ?
	           AND connection_state IN ('joining', 'reconnecting')`,
		[]any{updatedAtMillis, roomName, identity}
}

type pendingLiveParticipant struct {
	roomID              string
	userID              string
	clientLiveSessionID string
	connectionState     string
}

// Run reconciles transitional rows against LiveKit. Reconnecting users get a
// short seamless-recovery window. A client that dies after /live/join but
// before LiveKit connects has no participant_left webhook, so stale joining
// rows also need a longer bounded cleanup window.
func (h *Handler) Run(ctx context.Context) {
	ticker := time.NewTicker(liveReconnectSweepPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.cleanupExpiredLiveTransitions(ctx)
		}
	}
}

func (h *Handler) cleanupExpiredLiveTransitions(ctx context.Context) {
	now := time.Now()
	reconnectCutoff := now.Add(-liveReconnectGracePeriod).UnixMilli()
	joiningCutoff := now.Add(-liveJoiningGracePeriod).UnixMilli()
	rows, err := h.DB.QueryContext(
		ctx,
		expiredLiveTransitionsQuery,
		reconnectCutoff,
		joiningCutoff,
	)
	if err != nil {
		log.Printf("livekit webhook: list expired live transitions failed: %v", err)
		return
	}
	defer rows.Close()

	byRoom := make(map[string][]pendingLiveParticipant)
	for rows.Next() {
		var participant pendingLiveParticipant
		if err := rows.Scan(
			&participant.roomID,
			&participant.userID,
			&participant.clientLiveSessionID,
			&participant.connectionState,
		); err != nil {
			log.Printf("livekit webhook: scan expired live transition failed: %v", err)
			return
		}
		byRoom[participant.roomID] = append(byRoom[participant.roomID], participant)
	}
	if err := rows.Err(); err != nil {
		log.Printf("livekit webhook: iterate expired live transitions failed: %v", err)
		return
	}

	for roomID, participants := range byRoom {
		active, known := h.activeParticipantIdentities(ctx, roomID)
		if !known {
			// A transient LiveKit admin-API failure must not evict users from the
			// business roster. Leave the rows for the next sweep.
			continue
		}
		changed := false
		for _, participant := range participants {
			transitionCutoff := reconnectCutoff
			if participant.connectionState == "joining" {
				transitionCutoff = joiningCutoff
			}
			var res sql.Result
			if _, isActive := active[participant.userID]; isActive {
				res, err = h.DB.ExecContext(
					ctx,
					`UPDATE live_participants
					    SET connection_state = 'online', updated_at = ?
					  WHERE room_id = ? AND user_id = ? AND client_live_session_id = ?
					    AND connection_state = ?`,
					time.Now().UnixMilli(),
					participant.roomID,
					participant.userID,
					participant.clientLiveSessionID,
					participant.connectionState,
				)
			} else {
				res, err = h.DB.ExecContext(
					ctx,
					`DELETE FROM live_participants
					  WHERE room_id = ? AND user_id = ? AND client_live_session_id = ?
					    AND connection_state = ? AND updated_at <= ?`,
					participant.roomID,
					participant.userID,
					participant.clientLiveSessionID,
					participant.connectionState,
					transitionCutoff,
				)
			}
			if err != nil {
				log.Printf("livekit webhook: reconcile expired live transition failed: %v", err)
				continue
			}
			if affected, _ := res.RowsAffected(); affected > 0 {
				changed = true
			}
		}
		if changed {
			h.publish(roomID, "live_participants_reconciled", nil)
		}
	}
}

func (h *Handler) activeParticipantIdentities(
	ctx context.Context,
	roomID string,
) (map[string]struct{}, bool) {
	if h.RoomClient == nil ||
		h.Cfg == nil ||
		h.Cfg.LiveKitAPIKey == "" ||
		h.Cfg.LiveKitAPISecret == "" {
		return map[string]struct{}{}, true
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := h.RoomClient.ListParticipants(
		requestCtx,
		&livekit.ListParticipantsRequest{Room: roomID},
	)
	if err != nil {
		log.Printf("livekit webhook: list active participants failed: %v", err)
		return nil, false
	}
	active := make(map[string]struct{}, len(response.Participants))
	for _, participant := range response.Participants {
		active[participant.GetIdentity()] = struct{}{}
	}
	return active, true
}

func (h *Handler) publish(roomID, eventType string, extra map[string]any) {
	if h.PublishLive == nil {
		return
	}
	h.PublishLive(roomID, eventType, extra)
}
