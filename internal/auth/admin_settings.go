package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

type forceResetUserPasswordRequest struct {
	NewPassword string `json:"new_password" binding:"required,min=8,max=256"`
}

func (h *Handler) requireSuperuser(c *gin.Context) bool {
	if model.IsSuperuser(h.DB, getUserID(c)) {
		return true
	}
	errorJSON(c, http.StatusForbidden, "forbidden", "super user required")
	return false
}

func (h *Handler) getForcedUserSettings(c *gin.Context) {
	if !h.requireSuperuser(c) {
		return
	}
	target, err := model.GetUserByID(h.DB, c.Param("user_id"))
	if err != nil {
		errorJSON(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userResponse(target)})
}

func (h *Handler) getForcedUserAudioSettings(c *gin.Context) {
	if !h.requireSuperuser(c) {
		return
	}
	userID := c.Param("user_id")
	if _, err := model.GetUserByID(h.DB, userID); err != nil {
		errorJSON(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	settings, err := h.ensureAudioSettings(userID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read audio settings")
		return
	}
	c.JSON(http.StatusOK, gin.H{"audio_settings": audioSettingsResponse(settings)})
}

func (h *Handler) updateForcedUserAudioSettings(c *gin.Context) {
	if !h.requireSuperuser(c) {
		return
	}
	var req UpdateAudioSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !hasAudioSettingUpdate(req) {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "at least one audio setting is required")
		return
	}
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().UnixMilli()}
	if !appendVolumeUpdate(&sets, &args, "default_audio_input_volume", req.DefaultAudioInputVolume) ||
		!appendVolumeUpdate(&sets, &args, "default_audio_output_volume", req.DefaultAudioOutputVolume) ||
		!appendVolumeUpdate(&sets, &args, "live_mic_input_volume", req.LiveMicInputVolume) ||
		!appendVolumeUpdate(&sets, &args, "live_voice_output_volume", req.LiveVoiceOutputVolume) ||
		!appendVolumeUpdate(&sets, &args, "live_screen_share_output_volume", req.LiveScreenShareOutputVolume) ||
		!appendVolumeUpdate(&sets, &args, "live_music_output_volume", req.LiveMusicOutputVolume) {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "audio volume fields must be integers from 0 to 100")
		return
	}
	userID := c.Param("user_id")
	if _, err := model.GetUserByID(h.DB, userID); err != nil {
		errorJSON(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if _, err := h.ensureAudioSettings(userID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to prepare audio settings")
		return
	}
	args = append(args, userID)
	if _, err := h.DB.Exec(`UPDATE user_audio_settings SET `+strings.Join(sets, ", ")+` WHERE user_id = ?`, args...); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to update audio settings")
		return
	}
	settings, err := h.loadAudioSettings(userID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read audio settings")
		return
	}
	c.JSON(http.StatusOK, gin.H{"audio_settings": audioSettingsResponse(settings)})
}

func (h *Handler) forceResetUserPassword(c *gin.Context) {
	if !h.requireSuperuser(c) {
		return
	}
	var req forceResetUserPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "new password must be 8-256 characters")
		return
	}
	targetID := c.Param("user_id")
	target, err := model.GetUserByID(h.DB, targetID)
	if err != nil {
		errorJSON(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if target.IsSuperuser {
		errorJSON(c, http.StatusForbidden, "forbidden", "super user password cannot be reset here")
		return
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password hash failed")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password update failed")
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, time.Now().Unix(), targetID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password update failed")
		return
	}
	if _, err := tx.Exec(`UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, time.Now().Unix(), targetID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password update failed")
		return
	}
	if err := tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password update failed")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}
