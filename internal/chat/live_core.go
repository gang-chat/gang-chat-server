package chat

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	livekittoken "github.com/zhuangkaiyi/gang-chat/server/internal/livekit"
)

func (h *Handler) getLiveState(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	live, err := h.buildLiveState(roomID, nowMillis())
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read live state")
		return
	}
	c.JSON(http.StatusOK, gin.H{"live": live})
}

func (h *Handler) joinLive(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	var req joinLiveRequest
	rawBody, ok := h.bindJSON(c, &req)
	if !ok {
		return
	}
	if h.replayIdempotency(c, rawBody) {
		return
	}
	if req.ClientLiveSessionID == "" || !allowed(req.Source, "room_card_speaker", "live_header", "live_panel", "reconnect") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "client_live_session_id and valid source are required")
		return
	}

	now := nowMillis()
	liveSessionID := newID("live")
	_, err := h.DB.Exec(
		`INSERT INTO live_participants (
		   live_session_id, room_id, user_id, client_live_session_id, joined_at, updated_at,
		   mic_muted, headphones_muted, voice_blocked, camera_on, screen_sharing, connection_state
		 ) VALUES (?, ?, ?, ?, ?, ?, 1, 0, 0, 0, 0, 'joining')
		 ON CONFLICT(room_id, user_id) DO UPDATE SET
		   client_live_session_id = excluded.client_live_session_id,
		   updated_at = excluded.updated_at,
		   connection_state = 'joining'`,
		liveSessionID, roomID, userID, req.ClientLiveSessionID, now, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to join live")
		return
	}
	_, _ = h.DB.Exec(`UPDATE rooms SET updated_at = ? WHERE id = ?`, now, roomID)

	participant, err := h.liveParticipantForUser(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read live participant")
		return
	}
	live, err := h.buildLiveState(roomID, now)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read live state")
		return
	}

	token, expiresAt, err := h.liveKitToken(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusServiceUnavailable, "livekit_unavailable", "LiveKit cannot issue media sessions")
		return
	}

	h.PublishLiveSnapshot(roomID, "live_participant_joined", map[string]any{
		"user_id": userID,
	})

	h.idempotentJSON(c, http.StatusOK, rawBody, liveJoinResponse{
		LiveKit: liveKitInfo{
			ServerURL:      h.Cfg.LiveKitHost,
			Token:          token,
			TokenExpiresAt: formatTime(expiresAt),
			RoomName:       roomID,
		},
		Participant: participant,
		Live:        live,
	})
}

func (h *Handler) updateMyLiveState(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	var req updateLiveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.MicMuted == nil && req.HeadphonesMuted == nil && req.CameraOn == nil && req.ScreenSharing == nil && req.ConnectionState == nil {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "at least one live state field is required")
		return
	}
	if req.ConnectionState != nil && !allowed(*req.ConnectionState, "joining", "online", "reconnecting", "left") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid connection_state")
		return
	}

	var voiceBlocked int
	_ = h.DB.QueryRow(`SELECT voice_blocked FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&voiceBlocked)

	sets := []string{"updated_at = ?"}
	args := []any{nowMillis()}
	if req.MicMuted != nil {
		sets = append(sets, "mic_muted = ?")
		args = append(args, boolToInt(*req.MicMuted || voiceBlocked != 0))
	}
	if req.HeadphonesMuted != nil {
		sets = append(sets, "headphones_muted = ?")
		args = append(args, boolToInt(*req.HeadphonesMuted || voiceBlocked != 0))
	}
	if voiceBlocked != 0 {
		if req.MicMuted == nil {
			sets = append(sets, "mic_muted = 1")
		}
		if req.HeadphonesMuted == nil {
			sets = append(sets, "headphones_muted = 1")
		}
	}
	if req.CameraOn != nil {
		sets = append(sets, "camera_on = ?")
		args = append(args, boolToInt(*req.CameraOn))
	}
	if req.ScreenSharing != nil {
		sets = append(sets, "screen_sharing = ?")
		args = append(args, boolToInt(*req.ScreenSharing))
	}
	if req.ConnectionState != nil {
		sets = append(sets, "connection_state = ?")
		args = append(args, *req.ConnectionState)
	}
	args = append(args, roomID, userID)

	res, err := h.DB.Exec(
		`UPDATE live_participants SET `+strings.Join(sets, ", ")+` WHERE room_id = ? AND user_id = ?`,
		args...,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to update live state")
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		h.jsonError(c, http.StatusConflict, "conflict", "user is not in live")
		return
	}

	participant, err := h.liveParticipantForUser(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read live participant")
		return
	}

	h.PublishLiveSnapshot(roomID, "live_participant_updated", map[string]any{
		"user_id": userID,
	})

	c.JSON(http.StatusOK, gin.H{"participant": participant})
}

func (h *Handler) buildLiveState(roomID string, fallbackUpdatedAt int64) (liveState, error) {
	rows, err := h.DB.Query(
		`SELECT lp.live_session_id, lp.joined_at, lp.updated_at, lp.mic_muted,
		        lp.headphones_muted, lp.voice_blocked, lp.camera_on,
		        lp.screen_sharing, lp.connection_state, u.id, u.uid, u.username,
		        u.display_name, u.avatar_url, u.default_avatar_key
		 FROM live_participants lp
		 JOIN users u ON u.id = lp.user_id
		 WHERE lp.room_id = ?
		 ORDER BY lp.joined_at ASC`,
		roomID,
	)
	if err != nil {
		return liveState{}, err
	}
	defer rows.Close()

	participants := make([]liveParticipant, 0)
	updatedAt := fallbackUpdatedAt
	for rows.Next() {
		participant, participantUpdatedAt, err := scanLiveParticipant(rows)
		if err != nil {
			return liveState{}, err
		}
		if participantUpdatedAt > updatedAt {
			updatedAt = participantUpdatedAt
		}
		participants = append(participants, participant)
	}
	participants = h.annotateMusicParticipants(roomID, participants)
	return liveState{
		RoomID:           roomID,
		ParticipantCount: len(participants),
		Participants:     participants,
		UpdatedAt:        formatMillis(updatedAt),
	}, nil
}

func (h *Handler) liveParticipantForUser(roomID, userID string) (liveParticipant, error) {
	row := h.DB.QueryRow(
		`SELECT lp.live_session_id, lp.joined_at, lp.updated_at, lp.mic_muted,
		        lp.headphones_muted, lp.voice_blocked, lp.camera_on,
		        lp.screen_sharing, lp.connection_state, u.id, u.uid, u.username,
		        u.display_name, u.avatar_url, u.default_avatar_key
		 FROM live_participants lp
		 JOIN users u ON u.id = lp.user_id
		 WHERE lp.room_id = ? AND lp.user_id = ?`,
		roomID, userID,
	)
	participant, _, err := scanLiveParticipant(row)
	if err == nil {
		participants := h.annotateMusicParticipants(roomID, []liveParticipant{participant})
		participant = participants[0]
	}
	return participant, err
}

func (h *Handler) liveKitToken(roomID, userID string) (string, time.Time, error) {
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	canPublish, canSubscribe := h.liveKitMediaPermissions(roomID, userID)
	if h.Cfg.LiveKitAPIKey == "" || h.Cfg.LiveKitAPISecret == "" {
		return "dev-livekit-token", expiresAt, nil
	}
	token, err := livekittoken.GenerateJoinToken(livekittoken.TokenParams{
		APIKey:       h.Cfg.LiveKitAPIKey,
		APISecret:    h.Cfg.LiveKitAPISecret,
		Room:         roomID,
		Identity:     userID,
		Name:         currentUsernameFromDB(h.DB, userID),
		CanPublish:   canPublish,
		CanSubscribe: canSubscribe,
		TTL:          10 * time.Minute,
	})
	return token, expiresAt, err
}

func (h *Handler) liveKitMediaPermissions(roomID, userID string) (bool, bool) {
	var headphonesMuted, voiceBlocked int
	_ = h.DB.QueryRow(
		`SELECT headphones_muted, voice_blocked FROM live_participants WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&headphonesMuted, &voiceBlocked)
	canPublish := voiceBlocked == 0
	canSubscribe := voiceBlocked == 0 && headphonesMuted == 0
	return canPublish, canSubscribe
}
