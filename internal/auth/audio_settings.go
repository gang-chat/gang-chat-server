package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const defaultAudioVolume = 100

type audioSettingsRecord struct {
	DefaultAudioInputVolume     int
	DefaultAudioOutputVolume    int
	LiveMicInputVolume          int
	LiveVoiceOutputVolume       int
	LiveScreenShareOutputVolume int
	LiveMusicOutputVolume       int
	UpdatedAt                   int64
}

func (h *Handler) getAudioSettings(c *gin.Context) {
	settings, err := h.ensureAudioSettings(getUserID(c))
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read audio settings")
		return
	}
	c.JSON(http.StatusOK, gin.H{"audio_settings": audioSettingsResponse(settings)})
}

func (h *Handler) updateAudioSettings(c *gin.Context) {
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

	userID := getUserID(c)
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

func (h *Handler) ensureAudioSettings(userID string) (audioSettingsRecord, error) {
	now := time.Now().UTC().UnixMilli()
	_, err := h.DB.Exec(
		`INSERT OR IGNORE INTO user_audio_settings (
		   user_id, default_audio_input_volume, default_audio_output_volume,
		   live_mic_input_volume, live_voice_output_volume,
		   live_screen_share_output_volume, live_music_output_volume, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, defaultAudioVolume, defaultAudioVolume, defaultAudioVolume,
		defaultAudioVolume, defaultAudioVolume, defaultAudioVolume, now,
	)
	if err != nil {
		return audioSettingsRecord{}, err
	}
	return h.loadAudioSettings(userID)
}

func (h *Handler) loadAudioSettings(userID string) (audioSettingsRecord, error) {
	var rec audioSettingsRecord
	err := h.DB.QueryRow(
		`SELECT default_audio_input_volume, default_audio_output_volume,
		        live_mic_input_volume, live_voice_output_volume,
		        live_screen_share_output_volume, live_music_output_volume, updated_at
		 FROM user_audio_settings WHERE user_id = ?`,
		userID,
	).Scan(
		&rec.DefaultAudioInputVolume, &rec.DefaultAudioOutputVolume,
		&rec.LiveMicInputVolume, &rec.LiveVoiceOutputVolume,
		&rec.LiveScreenShareOutputVolume, &rec.LiveMusicOutputVolume, &rec.UpdatedAt,
	)
	return rec, err
}

func audioSettingsResponse(rec audioSettingsRecord) AudioSettingsResponse {
	return AudioSettingsResponse{
		DefaultAudioInputVolume:     rec.DefaultAudioInputVolume,
		DefaultAudioOutputVolume:    rec.DefaultAudioOutputVolume,
		LiveMicInputVolume:          rec.LiveMicInputVolume,
		LiveVoiceOutputVolume:       rec.LiveVoiceOutputVolume,
		LiveScreenShareOutputVolume: rec.LiveScreenShareOutputVolume,
		LiveMusicOutputVolume:       rec.LiveMusicOutputVolume,
		UpdatedAt:                   rfc3339(time.UnixMilli(rec.UpdatedAt)),
	}
}

func hasAudioSettingUpdate(req UpdateAudioSettingsRequest) bool {
	return req.DefaultAudioInputVolume != nil ||
		req.DefaultAudioOutputVolume != nil ||
		req.LiveMicInputVolume != nil ||
		req.LiveVoiceOutputVolume != nil ||
		req.LiveScreenShareOutputVolume != nil ||
		req.LiveMusicOutputVolume != nil
}

func appendVolumeUpdate(sets *[]string, args *[]any, column string, value *int) bool {
	if value == nil {
		return true
	}
	if *value < 0 || *value > 100 {
		return false
	}
	*sets = append(*sets, column+" = ?")
	*args = append(*args, *value)
	return true
}
