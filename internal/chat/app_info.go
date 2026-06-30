package chat

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const latestClientVersion = "0.4.0"

func (h *Handler) getAppVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"latest_version":            latestClientVersion,
		"minimum_supported_version": latestClientVersion,
	})
}
