package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
)

const ServerTimeHeader = "X-Gang-Server-Time"

func ServerTime() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header(ServerTimeHeader, time.Now().UTC().Format(time.RFC3339Nano))
		c.Next()
	}
}
