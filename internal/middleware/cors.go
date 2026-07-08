package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS sets cross-origin headers. allowedOrigins is the configured allow-list:
//   - empty or containing "*" → reflect any origin (legacy permissive behavior);
//     credentials are not used (Bearer tokens travel in the Authorization header,
//     not cookies), so reflecting "*" is acceptable for this API.
//   - otherwise → only echo Access-Control-Allow-Origin when the request's
//     Origin is in the list, so browsers reject disallowed origins.
func CORS(allowedOrigins ...string) gin.HandlerFunc {
	allowAny := len(allowedOrigins) == 0
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAny = true
			continue
		}
		if o != "" {
			allowed[o] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		originAllowed := false
		if allowAny {
			c.Header("Access-Control-Allow-Origin", "*")
			originAllowed = true
		} else if origin != "" {
			if _, ok := allowed[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
				originAllowed = true
			}
		}

		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept-Charset, Authorization, Idempotency-Key, X-Request-Id")
		c.Header("Access-Control-Expose-Headers", "X-Request-Id, Content-Disposition, "+ServerTimeHeader)
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			// Reject the preflight for disallowed origins so the browser blocks
			// the real request rather than silently letting it through.
			if !originAllowed && origin != "" {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
