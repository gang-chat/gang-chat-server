package chat

type roomSettingsRequest struct {
	Name                        *string `json:"name"`
	Description                 *string `json:"description"`
	AvatarAssetID               *string `json:"avatar_asset_id"`
	DefaultAvatarKey            *string `json:"default_avatar_key"`
	Visibility                  *string `json:"visibility"`
	JoinPolicy                  *string `json:"join_policy"`
	AIVoiceAnnounceEnabled      *bool   `json:"ai_voice_announce_enabled"`
	AIVoiceAnnouncementsEnabled *bool   `json:"ai_voice_announcements_enabled"`
	MessageRecallPolicy         *string `json:"message_recall_policy"`
	MessageRecallWindowSeconds  *int64  `json:"message_recall_window_seconds"`
}

type myRoomSettingsRequest struct {
	RemarkName         *string `json:"remark_name"`
	RoomDisplayName    *string `json:"room_display_name"`
	RoomAvatarAssetID  *string `json:"room_avatar_asset_id"`
	AvatarAssetID      *string `json:"avatar_asset_id"`
	DefaultAvatarKey   *string `json:"default_avatar_key"`
	NotificationLevel  *string `json:"notification_level"`
	NotificationPolicy *string `json:"notification_policy"`
}

type userIDRequest struct {
	UserID string `json:"user_id"`
}

type leaveRoomRequest struct {
	// ConfirmDeleteIfEmpty must be true for the leave to proceed when the
	// caller is the last member and leaving would delete the room. Without it
	// the server refuses (409) so the client can prompt for confirmation.
	ConfirmDeleteIfEmpty bool `json:"confirm_delete_if_empty"`
}

type roleRequest struct {
	Role string `json:"role"`
}

type decisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type joinRoomRequest struct {
	Reason string `json:"reason"`
}

type muteRequest struct {
	DurationSeconds *int64 `json:"duration_seconds"`
	Reason          string `json:"reason"`
}

type confirmRequest struct {
	Confirm bool   `json:"confirm"`
	Reason  string `json:"reason"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

type liveModerationRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type stickerPackRequest struct {
	Scope     string `json:"scope"`
	RoomID    string `json:"room_id"`
	Name      string `json:"name"`
	SortOrder *int   `json:"sort_order"`
}

type stickerRequest struct {
	AssetID   string `json:"asset_id"`
	Name      string `json:"name"`
	SortOrder *int   `json:"sort_order"`
}

type stickerReorderRequest struct {
	StickerIDs []string `json:"sticker_ids"`
}

type musicBoxEnqueueRequest struct {
	Source     string `json:"source"`
	TrackID    string `json:"track_id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMS *int64 `json:"duration_ms"`
}

type musicBoxControlRequest struct {
	Action string `json:"action"`
}

type saveStickerRequest struct {
	StickerID    string `json:"sticker_id"`
	TargetScope  string `json:"target_scope"`
	TargetPackID string `json:"target_pack_id"`
	Name         string `json:"name"`
	SortOrder    *int   `json:"sort_order"`
}
