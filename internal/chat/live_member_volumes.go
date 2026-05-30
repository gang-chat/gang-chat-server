package chat

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) listMyLiveMemberVolumes(c *gin.Context) {
	roomID := c.Param("room_id")
	listenerID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	rows, err := h.DB.Query(
		`SELECT v.room_id, v.volume, v.updated_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM live_member_volumes v
		 JOIN users u ON u.id = v.target_user_id
		 WHERE v.room_id = ? AND v.listener_user_id = ?
		 ORDER BY v.updated_at DESC`,
		roomID, listenerID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read member volumes")
		return
	}
	defer rows.Close()

	volumes := make([]liveMemberVolume, 0)
	for rows.Next() {
		volume, err := scanLiveMemberVolume(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read member volume")
			return
		}
		volumes = append(volumes, volume)
	}
	c.JSON(http.StatusOK, gin.H{"member_volumes": volumes})
}

func (h *Handler) updateMyLiveMemberVolume(c *gin.Context) {
	roomID := c.Param("room_id")
	listenerID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	targetID := c.Param("target_user_id")
	if targetID == "" || targetID == listenerID {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "valid target_user_id is required")
		return
	}
	if !h.isRoomMember(roomID, targetID) {
		h.jsonError(c, http.StatusNotFound, "not_found", "target member not found")
		return
	}

	var req updateLiveMemberVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Volume == nil || *req.Volume < 0 || *req.Volume > 100 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "volume must be an integer from 0 to 100")
		return
	}

	now := nowMillis()
	_, err := h.DB.Exec(
		`INSERT INTO live_member_volumes (room_id, listener_user_id, target_user_id, volume, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(room_id, listener_user_id, target_user_id) DO UPDATE SET
		   volume = excluded.volume,
		   updated_at = excluded.updated_at`,
		roomID, listenerID, targetID, *req.Volume, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to update member volume")
		return
	}

	volume, err := h.liveMemberVolumePayload(roomID, listenerID, targetID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read member volume")
		return
	}
	c.JSON(http.StatusOK, gin.H{"member_volume": volume})
}

func (h *Handler) liveMemberVolumePayload(roomID, listenerID, targetID string) (liveMemberVolume, error) {
	row := h.DB.QueryRow(
		`SELECT v.room_id, v.volume, v.updated_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM live_member_volumes v
		 JOIN users u ON u.id = v.target_user_id
		 WHERE v.room_id = ? AND v.listener_user_id = ? AND v.target_user_id = ?`,
		roomID, listenerID, targetID,
	)
	return scanLiveMemberVolume(row)
}

func scanLiveMemberVolume(row scanner) (liveMemberVolume, error) {
	var volume liveMemberVolume
	var targetID, uid, username string
	var displayName, avatarURL, defaultAvatar sql.NullString
	var updatedAt int64
	err := row.Scan(
		&volume.RoomID,
		&volume.Volume,
		&updatedAt,
		&targetID,
		&uid,
		&username,
		&displayName,
		&avatarURL,
		&defaultAvatar,
	)
	if err != nil {
		return liveMemberVolume{}, err
	}
	volume.TargetUser = summaryFromUserFields(targetID, uid, username, displayName, avatarURL, defaultAvatar)
	volume.UpdatedAt = formatMillis(updatedAt)
	return volume, nil
}
