package push

import (
	"database/sql"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	Store *Store
}

type deviceRegistrationRequest struct {
	Provider       string `json:"provider"`
	InstallationID string `json:"installation_id"`
	Token          string `json:"token"`
	Platform       string `json:"platform"`
	Enabled        *bool  `json:"enabled"`
}

type devicePreferenceRequest struct {
	Enabled *bool `json:"enabled"`
}

func RegisterRoutes(group *gin.RouterGroup, db *sql.DB) *Handler {
	h := &Handler{Store: &Store{DB: db}}
	group.PUT("/me/push-devices/:provider/:installation_id", h.upsert)
	group.PATCH("/me/push-devices/:provider/:installation_id", h.setEnabled)
	group.DELETE("/me/push-devices/:provider/:installation_id", h.delete)
	return h
}

func (h *Handler) upsert(c *gin.Context) {
	var req deviceRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "bad_request", "message": "invalid push device"}})
		return
	}
	req.Provider = strings.ToLower(strings.TrimSpace(c.Param("provider")))
	req.InstallationID = strings.TrimSpace(c.Param("installation_id"))
	req.Token = strings.TrimSpace(req.Token)
	req.Platform = strings.ToLower(strings.TrimSpace(req.Platform))
	if req.Platform == "" {
		req.Platform = "android"
	}
	if !validRegistration(req) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "validation_failed", "message": "invalid push device"}})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	err := h.Store.Upsert(
		c.GetString("user_id"),
		c.GetString("session_id"),
		DeviceRegistration{
			Provider:       req.Provider,
			InstallationID: req.InstallationID,
			Token:          req.Token,
			Platform:       req.Platform,
			Enabled:        enabled,
		},
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "push device update failed"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) setEnabled(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Param("provider")))
	installationID := strings.TrimSpace(c.Param("installation_id"))
	var req devicePreferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil ||
		!validProvider(provider) || installationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "bad_request", "message": "invalid push preference"}})
		return
	}
	found, err := h.Store.SetEnabled(
		c.GetString("user_id"),
		provider,
		installationID,
		*req.Enabled,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "push preference update failed"}})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": "push device not found"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) delete(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Param("provider")))
	installationID := strings.TrimSpace(c.Param("installation_id"))
	if !validProvider(provider) || installationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "bad_request", "message": "invalid push device"}})
		return
	}
	if err := h.Store.Delete(c.GetString("user_id"), provider, installationID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "push device delete failed"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func validRegistration(req deviceRegistrationRequest) bool {
	return validProvider(req.Provider) &&
		req.Platform == "android" &&
		req.InstallationID != "" &&
		utf8.RuneCountInString(req.InstallationID) <= 128 &&
		req.Token != "" &&
		utf8.RuneCountInString(req.Token) <= 1024
}

func validProvider(provider string) bool {
	return provider == "fcm" || provider == "oppo"
}
