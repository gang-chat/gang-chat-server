package chat

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
	"github.com/zhuangkaiyi/gang-chat/server/internal/push"
)

var (
	errInvalidPlaylistShare = errors.New("invalid playlist share")
	errInvalidTrackShare    = errors.New("invalid music track share")
)

func (h *Handler) listMessages(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	userID := currentUserID(c)
	if h.roomMessagesBlocked(roomID, userID) {
		c.JSON(http.StatusOK, gin.H{"messages": []message{}, "has_more": false, "next_before": nil})
		return
	}
	limit := parseLimit(c.Query("limit"), 50, 100)

	var rows *sql.Rows
	var err error
	before := c.Query("before")
	if before == "" {
		rows, err = h.DB.Query(
			`SELECT `+messageSelectColumnsSQL+`
			 FROM messages m
			 `+messageSenderUserJoinSQL+`
			 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
			 WHERE m.room_id = ? AND `+visibleMessageSQL("m")+`
			 ORDER BY m.created_at DESC, m.id DESC
			 LIMIT ?`,
			roomID, limit,
		)
	} else {
		var beforeCreatedAt int64
		err = h.DB.QueryRow(`SELECT created_at FROM messages WHERE id = ? AND room_id = ?`, before, roomID).Scan(&beforeCreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			h.jsonError(c, http.StatusBadRequest, "bad_request", "before message does not exist")
			return
		}
		if err == nil {
			rows, err = h.DB.Query(
				`SELECT `+messageSelectColumnsSQL+`
				 FROM messages m
				 `+messageSenderUserJoinSQL+`
				 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
				 WHERE m.room_id = ?
				   AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))
				   AND `+visibleMessageSQL("m")+`
				 ORDER BY m.created_at DESC, m.id DESC
				 LIMIT ?`,
				roomID, beforeCreatedAt, beforeCreatedAt, before, limit,
			)
		}
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list messages")
		return
	}
	defer rows.Close()

	messages := make([]message, 0)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read messages")
			return
		}
		msg = h.messageForViewer(msg, userID)
		messages = append(messages, msg)
	}
	reverseMessages(messages)

	hasMore := false
	var nextBefore *string
	if len(messages) > 0 {
		firstID := messages[0].ID
		firstCreatedAt := parseRFC3339Millis(messages[0].CreatedAt)
		var count int
		_ = h.DB.QueryRow(
			`SELECT COUNT(*) FROM messages m
			 WHERE m.room_id = ?
			   AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))
			   AND `+visibleMessageSQL("m"),
			roomID,
			firstCreatedAt,
			firstCreatedAt,
			firstID,
		).Scan(&count)
		hasMore = count > 0
		if hasMore {
			nextBefore = &firstID
		}
	}

	c.JSON(http.StatusOK, gin.H{"messages": messages, "has_more": hasMore, "next_before": nextBefore})
}

func (h *Handler) sendMessage(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	// requireRoomAccess lets a superuser ghost (no membership row) post for
	// announcements/moderation; their message carries their normal sender
	// identity. Normal non-members still get 404.
	if !h.requireRoomAccess(c, roomID) {
		return
	}

	var req sendMessageRequest
	rawBody, ok := h.bindJSON(c, &req)
	if !ok {
		return
	}
	if h.replayIdempotency(c, rawBody) {
		return
	}
	body := strings.TrimRight(req.Body, "\r\n")
	messageType := req.Type
	if messageType == "" {
		messageType = "text"
	}
	if !allowed(messageType, "text", "sticker", "audio", "file", "playlist", "music_track") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid message type")
		return
	}
	if req.ClientMessageID == "" || utf8.RuneCountInString(body) > 4000 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "client_message_id is required")
		return
	}
	if messageType == "text" && strings.TrimSpace(body) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "message body is required")
		return
	}
	if messageType != "text" && len(req.Attachments) == 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "attachments are required")
		return
	}
	if h.isTextMuted(roomID, userID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "user is muted in this room")
		return
	}
	if !h.validateMentions(c, roomID, req.Mentions) {
		return
	}
	if messageType == "playlist" {
		attachment, playlistName, err := h.sharedPlaylistAttachment(
			c.Request.Context(),
			roomID,
			userID,
			req.Attachments,
		)
		if err != nil {
			switch {
			case errors.Is(err, musicbox.ErrPlaylistNotFound):
				h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
			case errors.Is(err, errInvalidPlaylistShare):
				h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid playlist share")
			default:
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to share music playlist")
			}
			return
		}
		req.Attachments = []any{attachment}
		body = "[歌单] " + playlistName
	}
	if messageType == "music_track" {
		attachment, trackName, err := h.sharedMusicTrackAttachment(
			c.Request.Context(),
			userID,
			req.Attachments,
		)
		if err != nil {
			switch {
			case errors.Is(err, musicbox.ErrPlaylistNotFound):
				h.jsonError(c, http.StatusNotFound, "not_found", "music playlist or track not found")
			case errors.Is(err, errInvalidTrackShare):
				h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music track share")
			default:
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to share music track")
			}
			return
		}
		req.Attachments = []any{attachment}
		body = "[歌曲] " + trackName
	}
	mentionsJSON := mustJSON(req.Mentions)
	attachmentsJSON := mustJSON(req.Attachments)
	var quoteJSON any
	quoteMessageIDs := normalizedQuoteMessageIDs(req)
	if len(quoteMessageIDs) > 50 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "too many quoted messages")
		return
	}
	quotes := make([]messageQuote, 0, len(quoteMessageIDs))
	for _, quoteMessageID := range quoteMessageIDs {
		quoted, quoteErr := h.messageByIDForUser(quoteMessageID, userID)
		if quoteErr != nil || quoted.RoomID != roomID || quoted.IsRecalled || quoted.IsForceDeleted {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "quoted message is unavailable")
			return
		}
		quotes = append(quotes, messageQuote{
			MessageID:         quoted.ID,
			SenderDisplayName: quotedMessageSenderName(quoted),
			Body:              quotedMessageBodySnapshot(quoted),
			CreatedAt:         quoted.CreatedAt,
			PreviewAttachment: quotedMessagePreviewAttachment(quoted),
		})
	}
	if len(quotes) > 0 {
		quoteJSON = mustJSON(quotes)
	}

	now := nowMillis()
	messageID := newID("msg")
	_, err := insertMessageWithSenderSnapshotAndQuote(
		h.DB,
		messageID, roomID, userID, req.ClientMessageID, messageType, body,
		mentionsJSON, attachmentsJSON, quoteJSON, now,
	)
	if err != nil {
		existing, existingErr := h.messageByClientIDForUser(roomID, userID, req.ClientMessageID, userID)
		if existingErr == nil {
			h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"message": existing})
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to send message")
		return
	}
	_, _ = h.DB.Exec(`UPDATE rooms SET updated_at = ? WHERE id = ?`, now, roomID)

	msg, err := h.messageByIDForUser(messageID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message")
		return
	}
	// last_message lives in the room-list snapshot, so a new message refreshes
	// every member's list entry. Personal unread counts are added separately for
	// each recipient while the shared snapshot is published.
	h.publishRoomMessageUpdated(roomID, userID)
	h.publishRoomToUser(userID, roomID, "room_updated")
	if h.Push != nil {
		h.Push.Enqueue(push.RoomMessage{
			MessageID:  msg.ID,
			RoomID:     roomID,
			SenderID:   userID,
			SenderName: msg.Sender.DisplayName,
			Type:       msg.Type,
			Body:       msg.Body,
		})
	}
	if len(req.Mentions) > 0 {
		if recipients, err := h.appendMentionRoomNotifications(
			roomID,
			messageID,
			body,
			mentionsJSON,
			userID,
		); err == nil {
			for _, recipientID := range recipients {
				h.publishRoomNotificationsUpdated(recipientID)
			}
		}
	}
	h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"message": msg})
}

// sharedPlaylistAttachment replaces the untrusted client attachment (which
// contains only a personal playlist id) with an immutable server snapshot.
// This prevents forging another user's playlist and keeps old chat messages
// viewable after the source playlist is renamed or deleted.
func (h *Handler) sharedPlaylistAttachment(
	ctx context.Context,
	roomID, userID string,
	attachments []any,
) (gin.H, string, error) {
	if len(attachments) != 1 {
		return nil, "", errInvalidPlaylistShare
	}
	raw, ok := attachments[0].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(stringFromMap(raw, "type"))) != "playlist" {
		return nil, "", errInvalidPlaylistShare
	}
	if sourceMessageID := strings.TrimSpace(stringFromMap(raw, "source_message_id")); sourceMessageID != "" {
		return h.copiedMusicComponentAttachment(userID, sourceMessageID, "playlist")
	}
	if h.Playlists == nil {
		return nil, "", errInvalidPlaylistShare
	}
	playlistID := strings.TrimSpace(stringFromMap(raw, "playlist_id"))
	if playlistID == "" {
		return nil, "", errInvalidPlaylistShare
	}
	snapshot, err := h.Playlists.UserPlaylistSnapshot(ctx, userID, playlistID)
	if err != nil {
		return nil, "", err
	}
	creator, err := h.userSummaryForRoom(roomID, userID)
	if err != nil {
		return nil, "", err
	}
	items := make([]gin.H, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		items = append(items, musicPlaylistItemPayload(item))
	}
	playlist := musicPlaylistPayload(snapshot.Playlist)
	playlist["creator"] = creator
	playlist["items"] = items
	return gin.H{
		"type":     "playlist",
		"playlist": playlist,
	}, snapshot.Playlist.Name, nil
}

// sharedMusicTrackAttachment replaces the untrusted source playlist/item
// reference with an immutable track snapshot. A personal source must belong to
// the sender; a room source must still be accessible to the sender at send
// time. The destination room is intentionally independent from the source so
// a saved track can be shared across joined rooms.
func (h *Handler) sharedMusicTrackAttachment(
	ctx context.Context,
	userID string,
	attachments []any,
) (gin.H, string, error) {
	if len(attachments) != 1 {
		return nil, "", errInvalidTrackShare
	}
	raw, ok := attachments[0].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(stringFromMap(raw, "type"))) != "music_track" {
		return nil, "", errInvalidTrackShare
	}
	if sourceMessageID := strings.TrimSpace(stringFromMap(raw, "source_message_id")); sourceMessageID != "" {
		return h.copiedMusicComponentAttachment(userID, sourceMessageID, "music_track")
	}
	if h.Playlists == nil {
		return nil, "", errInvalidTrackShare
	}
	playlistID := strings.TrimSpace(stringFromMap(raw, "playlist_id"))
	itemID := strings.TrimSpace(stringFromMap(raw, "item_id"))
	scope := strings.ToLower(strings.TrimSpace(stringFromMap(raw, "playlist_scope")))
	if playlistID == "" || itemID == "" || !allowed(scope, "personal", "room") {
		return nil, "", errInvalidTrackShare
	}

	var snapshot musicbox.PlaylistItemsPage
	var err error
	if scope == "room" {
		sourceRoomID := strings.TrimSpace(stringFromMap(raw, "source_room_id"))
		if sourceRoomID == "" || (!h.isRoomMember(sourceRoomID, userID) && !h.isSuperuser(userID)) {
			return nil, "", musicbox.ErrPlaylistNotFound
		}
		snapshot, err = h.Playlists.RoomPlaylistSnapshot(ctx, sourceRoomID, playlistID)
	} else {
		snapshot, err = h.Playlists.UserPlaylistSnapshot(ctx, userID, playlistID)
	}
	if err != nil {
		return nil, "", err
	}
	for _, item := range snapshot.Items {
		if item.ID != itemID {
			continue
		}
		return gin.H{
			"type":  "music_track",
			"track": musicPlaylistItemPayload(item),
		}, item.Title, nil
	}
	return nil, "", musicbox.ErrPlaylistNotFound
}

// copiedMusicComponentAttachment reuses the immutable snapshot stored on a
// message the sender can still access. The client never resubmits snapshot
// fields, so a copied card cannot forge a playlist or song owned by somebody
// else. This also lets a copied component survive source playlist deletion.
func (h *Handler) copiedMusicComponentAttachment(
	userID, sourceMessageID, expectedType string,
) (gin.H, string, error) {
	source, err := h.messageByID(sourceMessageID)
	if err != nil || source.IsRecalled || source.IsForceDeleted ||
		strings.ToLower(strings.TrimSpace(source.Type)) != expectedType ||
		(!h.isRoomMember(source.RoomID, userID) && !h.isSuperuser(userID)) {
		if expectedType == "playlist" {
			return nil, "", errInvalidPlaylistShare
		}
		return nil, "", errInvalidTrackShare
	}
	payloadKey := "track"
	nameKey := "title"
	invalid := errInvalidTrackShare
	if expectedType == "playlist" {
		payloadKey = "playlist"
		nameKey = "name"
		invalid = errInvalidPlaylistShare
	}
	for _, value := range source.Attachments {
		attachment, ok := value.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(stringFromMap(attachment, "type"))) != expectedType {
			continue
		}
		payload, ok := attachment[payloadKey].(map[string]any)
		name := strings.TrimSpace(stringFromMap(payload, nameKey))
		if !ok || name == "" {
			return nil, "", invalid
		}
		return gin.H{
			"type":     expectedType,
			payloadKey: payload,
		}, name, nil
	}
	return nil, "", invalid
}

func (h *Handler) cloneSharedPlaylistToMe(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	if h.Playlists == nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "music playlists unavailable")
		return
	}
	msg, err := h.messageByIDForUser(c.Param("message_id"), userID)
	if err != nil || msg.RoomID != roomID || msg.Type != "playlist" || msg.IsRecalled || msg.IsForceDeleted {
		h.jsonError(c, http.StatusNotFound, "not_found", "shared playlist message not found")
		return
	}
	playlist, ok := sharedPlaylistSnapshotFromMessage(msg)
	if !ok {
		h.jsonError(c, http.StatusNotFound, "not_found", "shared playlist snapshot not found")
		return
	}
	tracks, ok := sharedPlaylistSnapshotTracks(playlist)
	if !ok {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "shared playlist snapshot is invalid")
		return
	}
	creatorName := quotedMessageSenderName(msg)
	if creator, creatorOK := playlist["creator"].(map[string]any); creatorOK {
		creatorName = firstNonEmptyString(
			stringFromMap(creator, "room_display_name"),
			stringFromMap(creator, "display_name"),
			stringFromMap(creator, "username"),
			creatorName,
		)
	}
	requestedName := creatorName + "的歌单 · " + strings.TrimSpace(stringFromMap(playlist, "name"))
	result, err := h.Playlists.CloneSnapshotToUserPlaylist(
		c.Request.Context(),
		userID,
		requestedName,
		tracks,
	)
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistLimit):
			h.jsonError(c, http.StatusConflict, "playlist_limit_reached", "playlist limit reached")
		case errors.Is(err, musicbox.ErrPlaylistItemLimit):
			h.jsonError(c, http.StatusConflict, "playlist_item_limit_reached", "playlist item limit reached")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to clone shared playlist")
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": musicPlaylistPayload(result)})
}

func sharedPlaylistSnapshotFromMessage(msg message) (map[string]any, bool) {
	for _, raw := range msg.Attachments {
		attachment, ok := raw.(map[string]any)
		if !ok || strings.ToLower(stringFromMap(attachment, "type")) != "playlist" {
			continue
		}
		playlist, ok := attachment["playlist"].(map[string]any)
		if ok && strings.TrimSpace(stringFromMap(playlist, "name")) != "" {
			return playlist, true
		}
	}
	return nil, false
}

func sharedPlaylistSnapshotTracks(playlist map[string]any) ([]musicbox.SnapshotTrack, bool) {
	rawItems, ok := playlist["items"].([]any)
	if !ok || len(rawItems) > musicbox.MaxPlaylistItems {
		return nil, false
	}
	tracks := make([]musicbox.SnapshotTrack, 0, len(rawItems))
	for _, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		source := strings.TrimSpace(stringFromMap(item, "source"))
		trackID := strings.TrimSpace(stringFromMap(item, "track_id"))
		title := strings.TrimSpace(stringFromMap(item, "title"))
		if source == "" || trackID == "" || title == "" {
			return nil, false
		}
		artists := make([]string, 0)
		if rawArtists, artistsOK := item["artists"].([]any); artistsOK {
			for _, rawArtist := range rawArtists {
				artist := strings.TrimSpace(stringValue(rawArtist))
				if artist != "" {
					artists = append(artists, artist)
				}
			}
		}
		tracks = append(tracks, musicbox.SnapshotTrack{
			Source:     source,
			TrackID:    trackID,
			Title:      title,
			Artist:     strings.Join(artists, "、"),
			DurationMS: int64FromMap(item, "duration_ms"),
		})
	}
	return tracks, true
}

func normalizedQuoteMessageIDs(req sendMessageRequest) []string {
	values := req.QuoteMessageIDs
	if len(values) == 0 && strings.TrimSpace(req.QuoteMessageID) != "" {
		values = []string{req.QuoteMessageID}
	}
	seen := make(map[string]bool, len(values))
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func quotedMessageSenderName(msg message) string {
	if msg.Type == systemMessageType {
		return ""
	}
	return firstNonEmptyString(
		dereferenceString(msg.Sender.RoomDisplayName),
		msg.Sender.DisplayName,
		msg.Sender.Username,
		"用户",
	)
}

func quotedMessageBodySnapshot(msg message) string {
	if msg.Type == systemMessageType {
		if body := systemMessageQuoteBody(msg); body != "" {
			return body
		}
	}
	if msg.Type == "text" {
		if body := strings.TrimSpace(msg.Body); body != "" {
			return body
		}
	}
	return lastMessageBodyPreview(msg.Type, msg.Body, mustJSON(msg.Attachments))
}

func systemMessageQuoteBody(msg message) string {
	subject := firstNonEmptyString(
		dereferenceString(msg.Sender.RoomDisplayName),
		msg.Sender.DisplayName,
		msg.Sender.Username,
		"用户",
	)
	body := strings.TrimSpace(msg.Body)
	for _, raw := range msg.Attachments {
		attachment, ok := raw.(map[string]any)
		if !ok || strings.ToLower(stringFromMap(attachment, "type")) != systemMessageType {
			continue
		}
		if target := systemAttachmentDisplayName(attachment, "target"); target != "" {
			subject = target
		} else if user := systemAttachmentDisplayName(attachment, "user"); user != "" {
			subject = user
		}
		actor := systemAttachmentDisplayName(attachment, "actor")
		profileActor := actor
		if profileActor == "" {
			profileActor = systemAttachmentDisplayName(attachment, "user")
		}
		switch stringFromMap(attachment, "event") {
		case systemEventRoomMemberJoined:
			return subject + " 加入了房间"
		case systemEventRoomMemberLeft:
			return subject + " 离开了房间"
		case systemEventRoomMemberRemoved:
			if actor == "" {
				return subject + " 被踢出了房间"
			}
			return subject + " 被 " + actor + " 踢出了房间"
		case systemEventLiveJoined:
			return subject + " 进入了语音频道"
		case systemEventLiveLeft:
			return subject + " 退出了语音频道"
		case systemEventRoomRoleChanged:
			fromRole := stringFromMap(attachment, "from_role")
			toRole := stringFromMap(attachment, "to_role")
			change := systemRoleChangeVerb(fromRole, toRole) + " " + systemRoleLabel(toRole)
			if actor != "" && !(fromRole == "owner" && toRole == "admin") {
				return subject + " 被 " + actor + " " + change
			}
			return subject + " " + change
		case systemEventRoomNameChanged:
			return systemRoomProfileChangeQuoteBody(
				"房间名称",
				profileActor,
				stringFromMap(attachment, "new_value"),
				false,
			)
		case systemEventRoomBioChanged:
			return systemRoomProfileChangeQuoteBody(
				"房间简介",
				profileActor,
				stringFromMap(attachment, "new_value"),
				true,
			)
		case systemEventRoomVisibilityChanged:
			return systemRoomProfileChangeQuoteBody(
				"房间可见性",
				profileActor,
				systemVisibilityLabel(stringFromMap(attachment, "new_value")),
				false,
			)
		case systemEventRoomJoinPolicyChanged:
			return systemRoomProfileChangeQuoteBody(
				"房间加入方式",
				profileActor,
				systemJoinPolicyLabel(stringFromMap(attachment, "new_value")),
				false,
			)
		}
	}
	if body == "" {
		return subject
	}
	return subject + " " + body
}

func systemRoomProfileChangeQuoteBody(subject, actor, value string, multiline bool) string {
	if value == "" {
		value = "（空）"
	}
	separator := " "
	if multiline {
		separator = "\n"
	}
	if actor == "" {
		return subject + " 修改为" + separator + value
	}
	return subject + " 被 " + actor + " 修改为" + separator + value
}

func quotedMessagePreviewAttachment(msg message) any {
	for _, raw := range msg.Attachments {
		attachment, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		attachmentType := strings.ToLower(stringFromMap(attachment, "type"))
		if attachmentType == "sticker" {
			return attachment
		}
		if attachmentType == "playlist" {
			return attachment
		}
		if attachmentType == "file" && strings.HasPrefix(strings.ToLower(attachmentMimeType(attachment)), "image/") {
			return attachment
		}
	}
	return nil
}

func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (h *Handler) markRead(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	if h.roomMessagesBlocked(roomID, userID) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "unread_count": 0})
		return
	}

	var req markReadRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.LastReadMessageID == "" {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "last_read_message_id is required")
		return
	}
	var candidateCreatedAt int64
	if err := h.DB.QueryRow(
		`SELECT created_at FROM messages WHERE id = ? AND room_id = ?`,
		req.LastReadMessageID,
		roomID,
	).Scan(&candidateCreatedAt); errors.Is(err, sql.ErrNoRows) {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "message does not exist")
		return
	} else if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message")
		return
	}

	now := nowMillis()
	// Ensure the per-account cursor exists first, then conditionally advance it.
	// The UPDATE predicate is evaluated while MySQL holds the room_reads row
	// lock, so concurrent devices can race safely: an older cursor can never
	// overwrite a newer one. The message id is the deterministic tie-breaker
	// used everywhere messages share the same millisecond timestamp.
	_, err := h.DB.Exec(
		`INSERT INTO room_reads (room_id, user_id, last_read_message_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE updated_at = updated_at`,
		roomID, userID, req.LastReadMessageID, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to mark room read")
		return
	}
	_, err = h.DB.Exec(
		`UPDATE room_reads rr
		 LEFT JOIN messages current_message ON current_message.id = rr.last_read_message_id
		 SET rr.last_read_message_id = ?, rr.updated_at = ?
		 WHERE rr.room_id = ? AND rr.user_id = ?
		   AND (
		     current_message.id IS NULL
		     OR current_message.created_at < ?
		     OR (current_message.created_at = ? AND current_message.id < ?)
		   )`,
		req.LastReadMessageID,
		now,
		roomID,
		userID,
		candidateCreatedAt,
		candidateCreatedAt,
		req.LastReadMessageID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to mark room read")
		return
	}

	unreadCount := h.unreadCount(roomID, userID)
	unreadMentionCount := h.unreadMentionCount(roomID, userID)
	// room_updated is account-addressed, so every live connection for this
	// user receives the committed cursor-derived counts, including the device
	// that initiated the request.
	h.publishRoomToUser(userID, roomID, "room_updated")
	c.JSON(http.StatusOK, gin.H{
		"ok":                   true,
		"unread_count":         unreadCount,
		"unread_mention_count": unreadMentionCount,
	})
}

func (h *Handler) messageByID(messageID string) (message, error) {
	return h.queryMessage(
		`SELECT `+messageSelectColumnsSQL+`
		 FROM messages m
		 `+messageSenderUserJoinSQL+`
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 WHERE m.id = ?`,
		messageID,
	)
}

func (h *Handler) messageByIDForUser(messageID, viewerID string) (message, error) {
	msg, err := h.messageByID(messageID)
	if err != nil {
		return message{}, err
	}
	return h.messageForViewer(msg, viewerID), nil
}

func (h *Handler) messageByClientID(roomID, userID, clientMessageID string) (message, error) {
	return h.queryMessage(
		`SELECT `+messageSelectColumnsSQL+`
		 FROM messages m
		 `+messageSenderUserJoinSQL+`
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 WHERE m.room_id = ? AND m.sender_user_id = ? AND m.client_message_id = ?`,
		roomID, userID, clientMessageID,
	)
}

func (h *Handler) messageByClientIDForUser(roomID, userID, clientMessageID, viewerID string) (message, error) {
	msg, err := h.messageByClientID(roomID, userID, clientMessageID)
	if err != nil {
		return message{}, err
	}
	return h.messageForViewer(msg, viewerID), nil
}

func (h *Handler) messageForViewer(msg message, viewerID string) message {
	h.hydrateMessageActionUsers(&msg)
	if !msg.IsRecalled || msg.Type != "text" {
		return msg
	}
	if viewerID != "" && h.canRecallMemberMessage(msg.RoomID, viewerID, msg.Sender.ID) {
		return msg
	}
	msg.Body = ""
	return msg
}

func (h *Handler) hydrateMessageActionUsers(msg *message) {
	if msg == nil {
		return
	}
	if msg.RecalledBy == nil && msg.recalledByUserID != "" {
		if summary, err := h.userSummaryForRoom(msg.RoomID, msg.recalledByUserID); err == nil {
			msg.RecalledBy = &summary
		}
	}
	if msg.ForceDeletedBy == nil && msg.forceDeletedByUserID != "" {
		if summary, err := h.userSummaryForRoom(msg.RoomID, msg.forceDeletedByUserID); err == nil {
			msg.ForceDeletedBy = &summary
		}
	}
}

func (h *Handler) queryMessage(query string, args ...any) (message, error) {
	var msg message
	var senderID, senderUID, senderUsername string
	var senderDisplayName, senderAvatarURL, senderDefaultAvatar sql.NullString
	var senderRoomDisplayName, senderRoomRole sql.NullString
	var mentionsJSON, attachmentsJSON string
	var quoteJSON sql.NullString
	var recalledAt, forceDeletedAt sql.NullInt64
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var isRecalled, isForceDeleted, senderIsSuperuser, senderIsDeleted int
	var createdAt int64
	err := h.DB.QueryRow(query, args...).Scan(
		&msg.ID, &msg.RoomID, &msg.ClientMessageID, &msg.Type, &msg.Body,
		&mentionsJSON, &attachmentsJSON, &quoteJSON, &isRecalled, &recalledAt, &recalledByUserID,
		&isForceDeleted, &forceDeletedAt, &forceDeletedByUserID, &createdAt,
		&senderID, &senderUID, &senderUsername, &senderDisplayName, &senderAvatarURL, &senderDefaultAvatar,
		&senderIsSuperuser, &senderRoomDisplayName, &senderRoomRole, &senderIsDeleted,
	)
	if err != nil {
		return message{}, err
	}
	msg.Sender = summaryFromUserFields(senderID, senderUID, senderUsername, senderDisplayName, senderAvatarURL, senderDefaultAvatar)
	msg.Sender.IsSuperuser = senderIsSuperuser != 0
	msg.Sender.IsDeleted = senderIsDeleted != 0
	msg.Sender.RoomDisplayName = nullableString(senderRoomDisplayName)
	if senderRoomRole.Valid && senderRoomRole.String != "" {
		msg.Sender.RoomRole = senderRoomRole.String
	}
	msg.Mentions = decodeJSONArray(mentionsJSON)
	msg.Attachments = decodeJSONArray(attachmentsJSON)
	msg.Quotes = decodeMessageQuotes(quoteJSON)
	if len(msg.Quotes) > 0 {
		msg.Quote = &msg.Quotes[0]
	}
	msg.IsRecalled = isRecalled != 0
	msg.IsForceDeleted = isForceDeleted != 0
	if recalledAt.Valid {
		v := formatMillis(recalledAt.Int64)
		msg.RecalledAt = &v
	}
	if recalledByUserID.Valid {
		msg.recalledByUserID = recalledByUserID.String
	}
	if forceDeletedAt.Valid {
		v := formatMillis(forceDeletedAt.Int64)
		msg.ForceDeletedAt = &v
	}
	if forceDeletedByUserID.Valid {
		msg.forceDeletedByUserID = forceDeletedByUserID.String
	}
	msg.CreatedAt = formatMillis(createdAt)
	return msg, nil
}
