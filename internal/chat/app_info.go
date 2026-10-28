package chat

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
)

// latestClientVersion answers /app/version until a release is published in
// the admin console.
const latestClientVersion = "0.4.0"

func (h *Handler) getAppVersion(c *gin.Context) {
	latest, err := releases.Latest(h.DB)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	version, minimum := latestClientVersion, latestClientVersion
	if latest != nil {
		version, minimum = latest.Version, latest.Version
		if latest.MinimumSupportedVersion != "" {
			minimum = latest.MinimumSupportedVersion
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"latest_version":            version,
		"minimum_supported_version": minimum,
	})
}
