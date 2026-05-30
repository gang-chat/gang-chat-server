package auth

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

type AuthMiddleware struct {
	DB        *sql.DB
	JWTSecret string
}

func (m *AuthMiddleware) Handle(c *gin.Context) {
	header := c.GetHeader("Authorization")
	if header == "" || !strings.HasPrefix(header, "Bearer ") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
			Error: ErrorBody{Code: "unauthorized", Message: "missing authorization header", RequestID: c.GetString("request_id")},
		})
		return
	}

	tokenStr := strings.TrimPrefix(header, "Bearer ")
	claims, err := VerifyAccessToken(tokenStr, m.JWTSecret)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
			Error: ErrorBody{Code: "unauthorized", Message: "invalid token", RequestID: c.GetString("request_id")},
		})
		return
	}

	// verify session is still active
	var revokedAt, status string
	err = m.DB.QueryRow(
		`SELECT COALESCE(CAST(us.revoked_at AS TEXT), ''), u.status
		 FROM user_sessions us JOIN users u ON u.id = us.user_id
		 WHERE us.id = ? AND us.expires_at > unixepoch()`,
		claims.Sid,
	).Scan(&revokedAt, &status)
	if err != nil || revokedAt != "" || status != "active" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
			Error: ErrorBody{Code: "unauthorized", Message: "session invalid", RequestID: c.GetString("request_id")},
		})
		return
	}

	// get username for convenience
	var username string
	_ = m.DB.QueryRow(`SELECT username FROM users WHERE id = ?`, claims.Sub).Scan(&username)

	c.Set("user_id", claims.Sub)
	c.Set("session_id", claims.Sid)
	c.Set("username", username)
	c.Next()
}

func getUserID(c *gin.Context) string    { return c.GetString("user_id") }
func getSessionID(c *gin.Context) string { return c.GetString("session_id") }
func getUsername(c *gin.Context) string  { return c.GetString("username") }

func getUserFromContext(c *gin.Context, db *sql.DB) (*model.User, bool) {
	userID := getUserID(c)
	u, err := model.GetUserByID(db, userID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
			Error: ErrorBody{Code: "unauthorized", Message: "user not found", RequestID: c.GetString("request_id")},
		})
		return nil, false
	}
	return u, true
}
