package chat

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}

type userSummary struct {
	ID               string           `json:"id"`
	UID              string           `json:"uid,omitempty"`
	Username         string           `json:"username"`
	DisplayName      string           `json:"display_name"`
	Gender           string           `json:"gender,omitempty"`
	AvatarURL        *string          `json:"avatar_url"`
	DefaultAvatarKey string           `json:"default_avatar_key"`
	RoomDisplayName  *string          `json:"room_display_name,omitempty"`
	RoomRole         string           `json:"room_role,omitempty"`
	Bio              *string          `json:"bio,omitempty"`
	IsSuperuser      bool             `json:"is_superuser,omitempty"`
	IsOnline         *bool            `json:"is_online,omitempty"`
	CommonRooms      []userCommonRoom `json:"common_rooms,omitempty"`
}

type userCommonRoom struct {
	ID               string  `json:"id"`
	RID              string  `json:"rid,omitempty"`
	Name             string  `json:"name"`
	RemarkName       *string `json:"remark_name"`
	AvatarURL        *string `json:"avatar_url"`
	DefaultAvatarKey string  `json:"default_avatar_key"`
	Visibility       string  `json:"visibility,omitempty"`
	RoomDisplayName  *string `json:"room_display_name,omitempty"`
	RoomRole         string  `json:"room_role,omitempty"`
}

type currentMember struct {
	User           userSummary `json:"user"`
	Role           string      `json:"role"`
	TextMutedUntil *string     `json:"text_muted_until"`
	JoinedAt       string      `json:"joined_at"`
}

type lastMessagePreview struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	SenderDisplayName string `json:"sender_display_name"`
	BodyPreview       string `json:"body_preview"`
	CreatedAt         string `json:"created_at"`
}

type roomCard struct {
	ID                   string              `json:"id"`
	RID                  string              `json:"rid,omitempty"`
	Name                 string              `json:"name"`
	RemarkName           *string             `json:"remark_name"`
	Description          string              `json:"description"`
	Visibility           string              `json:"visibility,omitempty"`
	JoinPolicy           string              `json:"join_policy,omitempty"`
	AvatarURL            *string             `json:"avatar_url"`
	DefaultAvatarKey     string              `json:"default_avatar_key"`
	MemberCount          int                 `json:"member_count"`
	MyRole               string              `json:"my_role,omitempty"`
	NotificationLevel    string              `json:"notification_level,omitempty"`
	NotificationPolicy   string              `json:"notification_policy,omitempty"`
	IsPinned             bool                `json:"is_pinned"`
	OnlineMemberCount    int                 `json:"online_member_count"`
	LiveParticipantCount int                 `json:"live_participant_count"`
	LiveAvatarPreview    []userSummary       `json:"live_avatar_preview"`
	LastMessage          *lastMessagePreview `json:"last_message"`
	UnreadCount          int                 `json:"unread_count"`
	UpdatedAt            string              `json:"updated_at"`
}

type publicRoom struct {
	ID                   string  `json:"id"`
	RID                  string  `json:"rid,omitempty"`
	Name                 string  `json:"name"`
	AvatarURL            *string `json:"avatar_url"`
	DefaultAvatarKey     string  `json:"default_avatar_key"`
	Visibility           string  `json:"visibility,omitempty"`
	JoinPolicy           string  `json:"join_policy,omitempty"`
	MemberCount          int     `json:"member_count"`
	OnlineMemberCount    int     `json:"online_member_count"`
	LiveParticipantCount int     `json:"live_participant_count"`
	Joined               bool    `json:"joined"`
	JoinState            string  `json:"join_state,omitempty"`
	UpdatedAt            string  `json:"updated_at"`
}

type searchRoomContext struct {
	ID               string  `json:"id"`
	RID              string  `json:"rid,omitempty"`
	Name             string  `json:"name"`
	RemarkName       *string `json:"remark_name,omitempty"`
	AvatarURL        *string `json:"avatar_url"`
	DefaultAvatarKey string  `json:"default_avatar_key"`
}

type messageSearchResult struct {
	Room    searchRoomContext `json:"room"`
	Message message           `json:"message"`
}

type roomMembership struct {
	Role              string  `json:"role,omitempty"`
	JoinedAt          string  `json:"joined_at"`
	RemarkName        *string `json:"remark_name"`
	RoomDisplayName   *string `json:"room_display_name"`
	NotificationLevel string  `json:"notification_level,omitempty"`
	IsPinned          bool    `json:"is_pinned"`
}

type roomPersonalProfile struct {
	DisplayName *string `json:"display_name"`
}

type roomDetail struct {
	ID                          string              `json:"id"`
	RID                         string              `json:"rid,omitempty"`
	Name                        string              `json:"name"`
	Description                 string              `json:"description"`
	AvatarURL                   *string             `json:"avatar_url"`
	DefaultAvatarKey            string              `json:"default_avatar_key"`
	Visibility                  string              `json:"visibility,omitempty"`
	JoinPolicy                  string              `json:"join_policy,omitempty"`
	AIVoiceAnnounceEnabled      bool                `json:"ai_voice_announce_enabled"`
	AIVoiceAnnouncementsEnabled bool                `json:"ai_voice_announcements_enabled"`
	MessageRecallPolicy         string              `json:"message_recall_policy,omitempty"`
	MessageRecallWindowSeconds  *int64              `json:"message_recall_window_seconds"`
	MemberCount                 int                 `json:"member_count"`
	OnlineMemberCount           int                 `json:"online_member_count"`
	CreatedBy                   *userSummary        `json:"created_by"`
	RemarkName                  *string             `json:"remark_name"`
	NotificationPolicy          string              `json:"notification_policy,omitempty"`
	IsPinned                    bool                `json:"is_pinned"`
	PersonalProfile             roomPersonalProfile `json:"personal_profile"`
	CanDeleteRoom               bool                `json:"can_delete_room"`
	MyMembership                roomMembership      `json:"my_membership"`
	Live                        liveState           `json:"live"`
	CreatedAt                   string              `json:"created_at"`
	UpdatedAt                   string              `json:"updated_at"`
}

type message struct {
	ID              string       `json:"id"`
	RoomID          string       `json:"room_id"`
	Sender          userSummary  `json:"sender"`
	ClientMessageID string       `json:"client_message_id"`
	Type            string       `json:"type"`
	Body            string       `json:"body"`
	Mentions        []any        `json:"mentions"`
	Attachments     []any        `json:"attachments"`
	IsRecalled      bool         `json:"is_recalled"`
	RecalledAt      *string      `json:"recalled_at"`
	RecalledBy      *userSummary `json:"recalled_by"`
	IsForceDeleted  bool         `json:"is_force_deleted"`
	ForceDeletedAt  *string      `json:"force_deleted_at"`
	ForceDeletedBy  *userSummary `json:"force_deleted_by"`
	CreatedAt       string       `json:"created_at"`
}

type liveParticipant struct {
	LiveSessionID       string      `json:"live_session_id"`
	User                userSummary `json:"user"`
	JoinedAt            string      `json:"joined_at"`
	MicMuted            bool        `json:"mic_muted"`
	MicBlocked          bool        `json:"mic_blocked"`
	HeadphonesMuted     bool        `json:"headphones_muted"`
	HeadphonesBlocked   bool        `json:"headphones_blocked"`
	HeadphonesListening bool        `json:"headphones_listening"`
	VoiceBlocked        bool        `json:"voice_blocked"`
	CameraOn            bool        `json:"camera_on"`
	ScreenSharing       bool        `json:"screen_sharing"`
	ConnectionState     string      `json:"connection_state"`
}

type liveMemberVolume struct {
	RoomID     string      `json:"room_id"`
	TargetUser userSummary `json:"target_user"`
	Volume     int         `json:"volume"`
	UpdatedAt  string      `json:"updated_at"`
}

type liveState struct {
	RoomID           string            `json:"room_id"`
	ParticipantCount int               `json:"participant_count"`
	Participants     []liveParticipant `json:"participants"`
	UpdatedAt        string            `json:"updated_at"`
}

type createRoomRequest struct {
	Name                        string  `json:"name"`
	Description                 string  `json:"description"`
	AvatarAssetID               *string `json:"avatar_asset_id"`
	DefaultAvatarKey            *string `json:"default_avatar_key"`
	Visibility                  string  `json:"visibility"`
	JoinPolicy                  string  `json:"join_policy"`
	AIVoiceAnnounceEnabled      *bool   `json:"ai_voice_announce_enabled"`
	AIVoiceAnnouncementsEnabled *bool   `json:"ai_voice_announcements_enabled"`
}

type sendMessageRequest struct {
	ClientMessageID string `json:"client_message_id"`
	Type            string `json:"type"`
	Body            string `json:"body"`
	Mentions        []any  `json:"mentions"`
	Attachments     []any  `json:"attachments"`
}

type markReadRequest struct {
	LastReadMessageID string `json:"last_read_message_id"`
}

type joinLiveRequest struct {
	ClientLiveSessionID string `json:"client_live_session_id"`
	Source              string `json:"source"`
}

type updateLiveRequest struct {
	MicMuted        *bool   `json:"mic_muted"`
	HeadphonesMuted *bool   `json:"headphones_muted"`
	CameraOn        *bool   `json:"camera_on"`
	ScreenSharing   *bool   `json:"screen_sharing"`
	ConnectionState *string `json:"connection_state"`
}

type updateLiveMemberVolumeRequest struct {
	Volume *int `json:"volume"`
}

type liveKitInfo struct {
	ServerURL      string `json:"server_url"`
	Token          string `json:"token"`
	TokenExpiresAt string `json:"token_expires_at"`
	RoomName       string `json:"room_name"`
}

type liveJoinResponse struct {
	LiveKit     liveKitInfo     `json:"livekit"`
	Participant liveParticipant `json:"participant"`
	Live        liveState       `json:"live"`
}

type roomRecord struct {
	ID                         string
	RID                        sql.NullString
	Name                       string
	AvatarURL                  sql.NullString
	DefaultAvatarKey           string
	CreatedByUserID            sql.NullString
	Visibility                 string
	JoinPolicy                 string
	AIVoiceAnnounceEnabled     int
	MessageRecallPolicy        string
	MessageRecallWindowSeconds sql.NullInt64
	Description                string
	CreatedAt                  int64
	UpdatedAt                  int64
}

func (h *Handler) userSummary(userID string) (userSummary, error) {
	var id, uid, username string
	var displayName, avatarURL, defaultAvatar sql.NullString
	err := h.DB.QueryRow(
		`SELECT id, uid, username, display_name, avatar_url, default_avatar_key FROM users WHERE id = ?`,
		userID,
	).Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar)
	if err != nil {
		return userSummary{}, err
	}
	return summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar), nil
}

func (h *Handler) profileUserSummary(userID, viewerID string) (userSummary, error) {
	var id, uid, username string
	var displayName, avatarURL, defaultAvatar, bio, gender sql.NullString
	var isSuperuser int
	err := h.DB.QueryRow(
		`SELECT id, uid, username, display_name, avatar_url, default_avatar_key, bio, gender, is_superuser
		 FROM users WHERE id = ? AND status = 'active'`,
		userID,
	).Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &bio, &gender, &isSuperuser)
	if err != nil {
		return userSummary{}, err
	}
	summary := summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar)
	summary.Bio = nullableString(bio)
	if gender.Valid {
		summary.Gender = gender.String
	}
	summary.IsSuperuser = isSuperuser != 0
	isOnline := h.isUserOnlineForViewer(id, viewerID)
	summary.IsOnline = &isOnline
	if !summary.IsSuperuser {
		rooms, err := h.userProfileRooms(id, viewerID, viewerID == id || h.isSuperuser(viewerID))
		if err != nil {
			return userSummary{}, err
		}
		summary.CommonRooms = rooms
	}
	return summary, nil
}

func (h *Handler) userProfileRooms(targetID, viewerID string, allRooms bool) ([]userCommonRoom, error) {
	query := `SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.visibility,
	                 target_rm.remark_name, target_rm.room_display_name, target_rm.role
	          FROM room_memberships target_rm
	          JOIN rooms r ON r.id = target_rm.room_id`
	args := []any{targetID}
	where := ` WHERE target_rm.user_id = ?`
	if !allRooms {
		query += ` JOIN room_memberships viewer_rm ON viewer_rm.room_id = target_rm.room_id`
		where += ` AND viewer_rm.user_id = ?`
		args = append(args, viewerID)
	}
	query += where + ` ORDER BY target_rm.joined_at DESC`

	rows, err := h.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms := make([]userCommonRoom, 0)
	for rows.Next() {
		var room userCommonRoom
		var rid, avatarURL, remarkName, roomDisplayName sql.NullString
		if err := rows.Scan(
			&room.ID,
			&rid,
			&room.Name,
			&avatarURL,
			&room.DefaultAvatarKey,
			&room.Visibility,
			&remarkName,
			&roomDisplayName,
			&room.RoomRole,
		); err != nil {
			return nil, err
		}
		room.RID = rid.String
		room.AvatarURL = nullableString(avatarURL)
		room.RemarkName = nullableString(remarkName)
		room.RoomDisplayName = nullableString(roomDisplayName)
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

func (h *Handler) jsonError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, errorResponse{
		Error: errorBody{Code: code, Message: message, RequestID: c.GetString("request_id")},
	})
}

func (h *Handler) bindJSON(c *gin.Context, dest any) ([]byte, bool) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid request body")
		return nil, false
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return nil, false
	}
	return raw, true
}

func (h *Handler) bindOptionalJSON(c *gin.Context, dest any) bool {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return true
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid request body")
		return false
	}
	if strings.TrimSpace(string(raw)) == "" || dest == nil {
		return true
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

func (h *Handler) replayIdempotency(c *gin.Context, rawBody []byte) bool {
	key := c.GetHeader("Idempotency-Key")
	if key == "" {
		return false
	}

	var storedHash string
	var status int
	var body string
	err := h.DB.QueryRow(
		`SELECT request_hash, response_status, response_body
		 FROM idempotency_keys
		 WHERE user_id = ? AND method = ? AND path = ? AND idempotency_key = ?`,
		currentUserID(c), c.Request.Method, c.Request.URL.Path, key,
	).Scan(&storedHash, &status, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read idempotency key")
		return true
	}
	if storedHash != bodyHash(rawBody) {
		h.jsonError(c, http.StatusConflict, "idempotency_conflict", "same idempotency key was reused with a different body")
		return true
	}

	c.Data(status, "application/json; charset=utf-8", []byte(body))
	return true
}

func (h *Handler) idempotentJSON(c *gin.Context, status int, rawBody []byte, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}

	key := c.GetHeader("Idempotency-Key")
	if key != "" {
		_, err = h.DB.Exec(
			`INSERT OR IGNORE INTO idempotency_keys (
			   user_id, method, path, idempotency_key, request_hash, response_status, response_body, created_at
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			currentUserID(c), c.Request.Method, c.Request.URL.Path, key, bodyHash(rawBody), status, string(data), nowMillis(),
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to store idempotency key")
			return
		}
	}
	c.Data(status, "application/json; charset=utf-8", data)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRoomRecord(rows *sql.Rows) (roomRecord, error) {
	var rec roomRecord
	err := rows.Scan(
		&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
		&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
		&rec.MessageRecallWindowSeconds, &rec.Description, &rec.CreatedAt, &rec.UpdatedAt,
	)
	return rec, err
}

func scanPublicRoomRecord(rows *sql.Rows) (roomRecord, bool, error) {
	var rec roomRecord
	var joined int
	err := rows.Scan(
		&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
		&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
		&rec.MessageRecallWindowSeconds, &rec.Description, &rec.CreatedAt, &rec.UpdatedAt, &joined,
	)
	return rec, joined != 0, err
}

func scanMessage(rows *sql.Rows) (message, error) {
	var msg message
	var senderID, senderUID, senderUsername string
	var senderDisplayName, senderAvatarURL, senderDefaultAvatar sql.NullString
	var senderRoomDisplayName, senderRoomRole sql.NullString
	var mentionsJSON, attachmentsJSON string
	var recalledAt, forceDeletedAt sql.NullInt64
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var isRecalled, isForceDeleted, senderIsSuperuser int
	var createdAt int64
	if err := rows.Scan(
		&msg.ID, &msg.RoomID, &msg.ClientMessageID, &msg.Type, &msg.Body,
		&mentionsJSON, &attachmentsJSON, &isRecalled, &recalledAt, &recalledByUserID,
		&isForceDeleted, &forceDeletedAt, &forceDeletedByUserID, &createdAt,
		&senderID, &senderUID, &senderUsername, &senderDisplayName, &senderAvatarURL, &senderDefaultAvatar,
		&senderIsSuperuser, &senderRoomDisplayName, &senderRoomRole,
	); err != nil {
		return message{}, err
	}
	msg.Sender = summaryFromUserFields(senderID, senderUID, senderUsername, senderDisplayName, senderAvatarURL, senderDefaultAvatar)
	msg.Sender.IsSuperuser = senderIsSuperuser != 0
	msg.Sender.RoomDisplayName = nullableString(senderRoomDisplayName)
	if senderRoomRole.Valid && senderRoomRole.String != "" {
		msg.Sender.RoomRole = senderRoomRole.String
	}
	msg.Mentions = decodeJSONArray(mentionsJSON)
	msg.Attachments = decodeJSONArray(attachmentsJSON)
	msg.IsRecalled = isRecalled != 0
	msg.IsForceDeleted = isForceDeleted != 0
	if recalledAt.Valid {
		v := formatMillis(recalledAt.Int64)
		msg.RecalledAt = &v
	}
	if forceDeletedAt.Valid {
		v := formatMillis(forceDeletedAt.Int64)
		msg.ForceDeletedAt = &v
	}
	msg.CreatedAt = formatMillis(createdAt)
	return msg, nil
}

func scanLiveParticipant(row scanner) (liveParticipant, int64, error) {
	var participant liveParticipant
	var userID, uid, username string
	var displayName, avatarURL, defaultAvatar sql.NullString
	var joinedAt, updatedAt int64
	var micMuted, micBlocked, headphonesMuted, headphonesBlocked, voiceBlocked, cameraOn, screenSharing int
	err := row.Scan(
		&participant.LiveSessionID,
		&joinedAt,
		&updatedAt,
		&micMuted,
		&micBlocked,
		&headphonesMuted,
		&headphonesBlocked,
		&voiceBlocked,
		&cameraOn,
		&screenSharing,
		&participant.ConnectionState,
		&userID,
		&uid,
		&username,
		&displayName,
		&avatarURL,
		&defaultAvatar,
	)
	if err != nil {
		return liveParticipant{}, 0, err
	}
	participant.User = summaryFromUserFields(userID, uid, username, displayName, avatarURL, defaultAvatar)
	participant.JoinedAt = formatMillis(joinedAt)
	participant.MicMuted = micMuted != 0
	participant.MicBlocked = micBlocked != 0
	participant.HeadphonesMuted = headphonesMuted != 0
	participant.HeadphonesBlocked = headphonesBlocked != 0
	participant.VoiceBlocked = voiceBlocked != 0
	participant.HeadphonesListening = !participant.HeadphonesMuted && !participant.HeadphonesBlocked
	participant.CameraOn = cameraOn != 0
	participant.ScreenSharing = screenSharing != 0
	return participant, updatedAt, nil
}

func currentUserID(c *gin.Context) string {
	return c.GetString("user_id")
}

func summaryFromUser(id, username string) userSummary {
	return userSummary{
		ID:               id,
		Username:         username,
		DisplayName:      username,
		AvatarURL:        nil,
		DefaultAvatarKey: "blue-3",
	}
}

func summaryFromUserFields(id, uid, username string, displayName, avatarURL, defaultAvatar sql.NullString) userSummary {
	name := username
	if displayName.Valid && displayName.String != "" {
		name = displayName.String
	}
	avatarKey := "blue-3"
	if defaultAvatar.Valid && defaultAvatar.String != "" {
		avatarKey = defaultAvatar.String
	}
	return userSummary{
		ID:               id,
		UID:              uid,
		Username:         username,
		DisplayName:      name,
		AvatarURL:        nullableString(avatarURL),
		DefaultAvatarKey: avatarKey,
	}
}

func currentUsernameFromDB(db *sql.DB, userID string) string {
	var username string
	_ = db.QueryRow(`SELECT username FROM users WHERE id = ?`, userID).Scan(&username)
	return username
}

func parseLimit(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func nowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

func formatMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseRFC3339Millis(value string) int64 {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return t.UTC().UnixMilli()
}

func newID(prefix string) string {
	return idgen.New(prefix)
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func defaultRoomAvatar(roomID string) string {
	sum := 0
	for _, ch := range roomID {
		sum += int(ch)
	}
	return "room-" + strconv.Itoa(sum%5+1)
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullableStringFromText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (h *Handler) isUserOnline(userID string) bool {
	if h.Bus == nil || userID == "" {
		return false
	}
	_, ok := h.Bus.OnlineUserIDs()[userID]
	return ok
}

func (h *Handler) isUserOnlineForViewer(userID, viewerID string) bool {
	if userID != "" && userID == viewerID {
		return true
	}
	return h.isUserOnline(userID)
}

func allowed(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func normalizeRoomNotificationPolicy(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "all":
		return "all", true
	case "silent", "quiet", "no_alert", "no_alerts", "receive_silent",
		"mention", "mentions", "only_mentions", "mention_only",
		"mute", "muted", "do_not_disturb", "dnd":
		return "silent", true
	case "block", "blocked", "ignore", "ignored":
		return "blocked", true
	default:
		return "", false
	}
}

func preview(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}

func reverseMessages(messages []message) {
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
}

func mustJSON(value any) string {
	if value == nil {
		return "[]"
	}
	data, err := json.Marshal(value)
	if err != nil || string(data) == "null" {
		return "[]"
	}
	return string(data)
}

func decodeJSONArray(raw string) []any {
	if raw == "" {
		return []any{}
	}
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []any{}
	}
	return out
}
