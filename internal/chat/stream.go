package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
)

// liveStream is the single server -> client realtime channel. Each client
// holds one long-lived SSE connection here; the server pushes a snapshot
// whenever something the user cares about changes (live participants,
// moderation, LiveKit disconnects, ...). Clients dispatch on the SSE event
// name and blindly overwrite their local state with the payload.
func (h *Handler) liveStream(c *gin.Context) {
	userID := currentUserID(c)
	if userID == "" {
		h.jsonError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if h.Bus == nil {
		h.jsonError(c, http.StatusServiceUnavailable, "stream_unavailable", "realtime stream is not enabled")
		return
	}

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // tell nginx not to buffer
	w.WriteHeader(http.StatusOK)
	w.Flush()

	rooms, err := h.userRoomIDs(userID)
	if err != nil {
		// We've already sent 200 + headers, so we can't switch to an error
		// status. Carry on with an empty room set; the client's reconnect
		// snapshot pull will heal anything we miss.
		rooms = nil
	}

	sub := h.Bus.Subscribe(userID)
	sub.SetRooms(rooms)
	defer sub.Close()

	// Tell the client the stream is live and hand it the server clock so it
	// can sanity-check its own timers.
	if err := writeSSE(w, "ready", gin.H{"server_time": formatMillis(nowMillis())}); err != nil {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := c.Request.Context()
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			if err := writeSSE(w, ev.Type, ev); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := w.WriteString(": keep-alive\n\n"); err != nil {
				return
			}
			w.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// writeSSE serializes payload as one SSE event and flushes. A write error
// means the peer hung up.
func writeSSE(w gin.ResponseWriter, eventName string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, data); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// userRoomIDs returns the SSE subscription's interest set at connect time:
// every room the user is a member of, plus any room where they're currently a
// live participant. The latter matters for a superuser ghost, who joins a
// room's voice channel without a membership row — without it they'd receive no
// live snapshots (those fan out via PublishRoom, gated by this set). On
// reconnect the set is rebuilt, and live join itself publishes a fresh
// snapshot, so transient gaps self-heal.
func (h *Handler) userRoomIDs(userID string) ([]string, error) {
	rows, err := h.DB.Query(
		`SELECT room_id FROM room_memberships WHERE user_id = ?
		 UNION
		 SELECT room_id FROM live_participants WHERE user_id = ?`,
		userID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PublishLiveSnapshot pushes a fresh full live state to every SSE subscriber
// of roomID. The snapshot is always rebuilt from the DB so clients can
// blindly overwrite their state instead of merging deltas. Best-effort:
// errors (including a nil bus) are swallowed so they never block the HTTP
// write path that triggered them. extra fields are merged into the payload.
func (h *Handler) PublishLiveSnapshot(roomID, eventType string, extra map[string]any) {
	if h == nil || h.Bus == nil || roomID == "" {
		return
	}
	live, err := h.buildLiveState(roomID, nowMillis())
	if err != nil {
		return
	}
	preview, count, err := h.livePreview(roomID)
	if err != nil {
		return
	}
	payload := map[string]any{
		"room_id":           roomID,
		"live":              live,
		"participant_count": count,
		"preview":           preview,
	}
	for k, v := range extra {
		payload[k] = v
	}
	h.Bus.PublishRoom(roomID, eventbus.Event{
		Type:   eventType,
		RoomID: roomID,
		Data:   payload,
	})
}
