package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

type Handler struct {
	DB               *sql.DB
	Cfg              *config.Config
	Limit            *RateLimiter
	LocationResolver *SessionLocationResolver
}

func NewHandler(db *sql.DB, cfg *config.Config) *Handler {
	locationResolver, err := NewSessionLocationResolver(cfg.GeoIPDatabasePath)
	if err != nil {
		panic(err)
	}
	return &Handler{
		DB:               db,
		Cfg:              cfg,
		Limit:            NewRateLimiter(cfg.LoginMaxAttempts, cfg.LoginWindowSeconds),
		LocationResolver: locationResolver,
	}
}

func RegisterRoutes(g *gin.RouterGroup, db *sql.DB, cfg *config.Config) {
	h := NewHandler(db, cfg)
	if err := h.ensureSuperUser(); err != nil {
		panic(err)
	}
	auth := g.Group("/auth")

	auth.POST("/register", h.register)
	auth.POST("/login", h.login)
	auth.POST("/refresh", h.refresh)
	auth.POST("/logout", h.logout)
	g.GET("/me", h.Authed(), h.me)
	auth.GET("/me", h.Authed(), h.me)
	auth.POST("/password", h.Authed(), h.changePassword)
	auth.GET("/sessions", h.Authed(), h.listSessions)
	auth.DELETE("/sessions/:id", h.Authed(), h.revokeSession)

	g.PATCH("/users/me/account", h.Authed(), h.updateAccount)
	g.PATCH("/users/me/profile", h.Authed(), h.updateProfile)
	g.GET("/users/me/audio-settings", h.Authed(), h.getAudioSettings)
	g.PATCH("/users/me/audio-settings", h.Authed(), h.updateAudioSettings)
	g.DELETE("/users/me/account", h.Authed(), h.deleteAccount)
	g.PATCH("/users/:user_id/settings", h.Authed(), h.forceUpdateUserSettings)
	g.GET("/users/search", h.Authed(), h.searchUsers)
	g.GET("/users/me/playlists", h.Authed(), h.listPersonalPlaylists)
	g.POST("/users/me/playlists", h.Authed(), h.createPersonalPlaylist)
	g.PATCH("/users/me/playlists/:playlist_id", h.Authed(), h.updatePersonalPlaylist)
	g.DELETE("/users/me/playlists/:playlist_id", h.Authed(), h.deletePersonalPlaylist)
	g.POST("/users/me/playlists/:playlist_id/tracks", h.Authed(), h.addPersonalPlaylistTrack)
	g.PATCH("/users/me/playlists/:playlist_id/tracks/:track_id", h.Authed(), h.updatePersonalPlaylistTrack)
	g.DELETE("/users/me/playlists/:playlist_id/tracks/:track_id", h.Authed(), h.deletePersonalPlaylistTrack)
}

func (h *Handler) Authed() gin.HandlerFunc {
	mw := &AuthMiddleware{DB: h.DB, JWTSecret: h.Cfg.JWTSecret}
	return mw.Handle
}

func (h *Handler) register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if !isValidUsername(req.Username) {
		errorJSON(c, http.StatusBadRequest, "bad_request", "username must be 3-32 chars, alphanumeric/underscore/hyphen")
		return
	}
	if !strings.Contains(req.Email, "@") || !strings.Contains(req.Email, ".") {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid email")
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password hash failed")
		return
	}

	id := uuid.New().String()
	usernameNorm := strings.ToLower(req.Username)
	emailNorm := strings.ToLower(req.Email)

	_, err = model.CreateUser(h.DB, id, req.Username, usernameNorm, req.Email, emailNorm, hash)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			errorJSON(c, http.StatusConflict, "conflict", "username or email already taken")
			return
		}
		errorJSON(c, http.StatusInternalServerError, "internal_error", "create user failed")
		return
	}

	resp, err := h.issueAuthResponse(id, c)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "token issue failed")
		return
	}
	c.JSON(http.StatusCreated, resp)
}

func (h *Handler) login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	loginNorm := strings.ToLower(req.Login)
	if !h.Limit.Check(loginNorm) {
		errorJSON(c, http.StatusTooManyRequests, "rate_limited", "too many failed login attempts")
		return
	}

	user, err := model.GetUserByUsernameOrEmail(h.DB, req.Login)
	if err != nil {
		h.Limit.RecordFailure(loginNorm)
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}
	if user.Status != "active" {
		h.Limit.RecordFailure(loginNorm)
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}

	if user.PasswordHash == nil {
		h.Limit.RecordFailure(loginNorm)
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}

	ok, err := VerifyPassword(req.Password, *user.PasswordHash)
	if err != nil || !ok {
		h.Limit.RecordFailure(loginNorm)
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}

	h.Limit.Clear(loginNorm)

	resp, err := h.issueAuthResponse(user.ID, c)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "token issue failed")
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) refresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	session, err := model.GetSessionByRefreshToken(h.DB, req.RefreshToken)
	if err != nil {
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "invalid refresh token")
		return
	}

	if session.RevokedAt != nil {
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "session revoked")
		return
	}
	if session.ExpiresAt < time.Now().Unix() {
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "session expired")
		return
	}

	user, err := model.GetUserByID(h.DB, session.UserID)
	if err != nil || user.Status != "active" {
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "user inactive")
		return
	}

	newRefresh, err := generateRefreshToken()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "token generation failed")
		return
	}

	if err := model.RotateRefreshToken(h.DB, session.ID, newRefresh); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "token rotation failed")
		return
	}

	accessToken, err := IssueAccessToken(user.ID, session.ID, h.Cfg.JWTSecret, h.Cfg.AccessTokenTTLSeconds)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "access token failed")
		return
	}

	c.JSON(http.StatusOK, AuthResponse{
		AccessToken:          accessToken,
		AccessTokenExpiresAt: rfc3339(time.Now().Add(time.Duration(h.Cfg.AccessTokenTTLSeconds) * time.Second)),
		RefreshToken:         newRefresh,
		ExpiresIn:            h.Cfg.AccessTokenTTLSeconds,
		User:                 userResponse(user),
	})
}

func (h *Handler) logout(c *gin.Context) {
	var req LogoutRequest
	_ = c.ShouldBindJSON(&req)

	if req.RefreshToken != nil && *req.RefreshToken != "" {
		_ = model.RevokeSessionByRefreshToken(h.DB, *req.RefreshToken)
	} else {
		sid := getSessionID(c)
		_ = model.RevokeSession(h.DB, sid)
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) me(c *gin.Context) {
	user, ok := getUserFromContext(c, h.DB)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, userResponse(user))
}

func (h *Handler) changePassword(c *gin.Context) {
	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	user, ok := getUserFromContext(c, h.DB)
	if !ok {
		return
	}

	if user.PasswordHash == nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "no password set")
		return
	}

	ok, err := VerifyPassword(req.CurrentPassword, *user.PasswordHash)
	if err != nil || !ok {
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "current password incorrect")
		return
	}

	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password hash failed")
		return
	}

	if err := model.UpdatePassword(h.DB, user.ID, newHash); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password update failed")
		return
	}

	revokeOthers := true
	if req.RevokeOtherSessions != nil {
		revokeOthers = *req.RevokeOtherSessions
	}
	if revokeOthers {
		sid := getSessionID(c)
		_ = model.RevokeAllOtherSessions(h.DB, user.ID, sid)
	}

	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) listSessions(c *gin.Context) {
	userID := getUserID(c)
	sid := getSessionID(c)
	sessions, err := model.ListRecentSessions(h.DB, userID, 20)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}

	resp := make([]SessionResponse, 0, len(sessions))
	for _, s := range sessions {
		resp = append(resp, SessionResponse{
			ID:         s.ID,
			UserAgent:  s.UserAgent,
			IPAddress:  s.IPAddress,
			Location:   h.sessionLocation(s.IPAddress),
			CreatedAt:  s.CreatedAt,
			LastUsedAt: s.LastUsedAt,
			ExpiresAt:  s.ExpiresAt,
			RevokedAt:  s.RevokedAt,
			IsCurrent:  s.ID == sid,
		})
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) revokeSession(c *gin.Context) {
	sessionID := c.Param("id")
	currentSID := getSessionID(c)

	if sessionID == currentSID {
		errorJSON(c, http.StatusBadRequest, "bad_request", "cannot revoke current session, use logout")
		return
	}

	err := model.RevokeSession(h.DB, sessionID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "revoke failed")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) issueAuthResponse(userID string, c *gin.Context) (*AuthResponse, error) {
	sessionID := uuid.New().String()
	refreshToken, err := generateRefreshToken()
	if err != nil {
		return nil, err
	}

	ua := c.GetHeader("User-Agent")
	ip := c.ClientIP()
	expiresAt := time.Now().Add(time.Duration(h.Cfg.RefreshTokenTTLSeconds) * time.Second).Unix()

	if err := model.CreateSession(h.DB, sessionID, userID, model.HashTokenForCreate(refreshToken), &ua, &ip, expiresAt); err != nil {
		return nil, err
	}

	accessToken, err := IssueAccessToken(userID, sessionID, h.Cfg.JWTSecret, h.Cfg.AccessTokenTTLSeconds)
	if err != nil {
		return nil, err
	}

	user, err := model.GetUserByID(h.DB, userID)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		AccessToken:          accessToken,
		AccessTokenExpiresAt: rfc3339(time.Now().Add(time.Duration(h.Cfg.AccessTokenTTLSeconds) * time.Second)),
		RefreshToken:         refreshToken,
		ExpiresIn:            h.Cfg.AccessTokenTTLSeconds,
		User:                 userResponse(user),
	}, nil
}

func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

func isValidUsername(s string) bool {
	return usernameRegex.MatchString(s)
}

func errorJSON(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{
		Error: ErrorBody{Code: code, Message: message, RequestID: c.GetString("request_id")},
	})
}

func userResponse(user *model.User) UserResponse {
	displayName := user.DisplayName.String
	if displayName == "" {
		displayName = user.Username
	}
	defaultAvatar := user.DefaultAvatarKey.String
	if defaultAvatar == "" {
		defaultAvatar = "blue-3"
	}
	bio := user.Bio.String
	gender := user.Gender
	if gender == "" {
		gender = "secret"
	}
	var avatarURL *string
	if user.AvatarURL.Valid {
		avatarURL = &user.AvatarURL.String
	}
	emailVerified := user.EmailVerified
	var usernameUpdatedAt *string
	if user.UsernameUpdatedAt.Valid && user.UsernameUpdatedAt.Int64 > 0 {
		v := rfc3339(time.Unix(user.UsernameUpdatedAt.Int64, 0))
		usernameUpdatedAt = &v
	}
	canChangeTime := time.Unix(user.CreatedAt, 0)
	if user.UsernameUpdatedAt.Valid && user.UsernameUpdatedAt.Int64 > user.CreatedAt {
		canChangeTime = time.Unix(user.UsernameUpdatedAt.Int64, 0).Add(24 * time.Hour)
	}
	canChangeAt := rfc3339(canChangeTime)
	return UserResponse{
		ID:                  user.ID,
		UID:                 user.UID.String,
		Username:            user.Username,
		DisplayName:         displayName,
		Bio:                 bio,
		Gender:              gender,
		Email:               user.Email,
		EmailVerified:       &emailVerified,
		EmailPublic:         user.EmailPublic,
		PhoneNumber:         user.PhoneNumber.String,
		PhoneNumberPublic:   user.PhoneNumberPublic,
		AvatarURL:           avatarURL,
		DefaultAvatarKey:    defaultAvatar,
		IsSuperuser:         user.IsSuperuser,
		Status:              user.Status,
		UsernameUpdatedAt:   usernameUpdatedAt,
		CanChangeUsernameAt: &canChangeAt,
		CreatedAt:           rfc3339(time.Unix(user.CreatedAt, 0)),
	}
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func (h *Handler) sessionLocation(ip *string) string {
	if h == nil || h.LocationResolver == nil {
		return (&SessionLocationResolver{}).Location(ip)
	}
	return h.LocationResolver.Location(ip)
}
