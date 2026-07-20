package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/apierrors"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

type Handler struct {
	DB                      *sql.DB
	Cfg                     *config.Config
	Limit                   *RateLimiter
	LocationResolver        *SessionLocationResolver
	VerificationEmailSender VerificationEmailSender
	Bus                     *eventbus.Bus
}

func NewHandler(db *sql.DB, cfg *config.Config, bus *eventbus.Bus) *Handler {
	locationResolver, err := NewSessionLocationResolver(cfg.GeoIPDatabasePath)
	if err != nil {
		// A missing/corrupt GeoIP database must not take down the whole
		// server. Location() already degrades to "unknown" when the reader
		// is nil, so fall back to an empty resolver and log loudly instead.
		log.Printf("auth: GeoIP database unavailable (%v); session locations will be unknown", err)
		locationResolver = &SessionLocationResolver{}
	}
	return &Handler{
		DB:                      db,
		Cfg:                     cfg,
		Limit:                   NewRateLimiter(cfg.LoginMaxAttempts, cfg.LoginWindowSeconds),
		LocationResolver:        locationResolver,
		VerificationEmailSender: newVerificationEmailSender(cfg),
		Bus:                     bus,
	}
}

func RegisterRoutes(g *gin.RouterGroup, db *sql.DB, cfg *config.Config, bus *eventbus.Bus) *Handler {
	h := NewHandler(db, cfg, bus)
	if err := h.ensureSuperUser(); err != nil {
		panic(err)
	}
	if err := h.ensurePasswordResetSchema(); err != nil {
		panic(err)
	}
	if err := h.ensureEmailVerificationSchema(); err != nil {
		panic(err)
	}
	auth := g.Group("/auth")

	auth.POST("/register", h.register)
	auth.POST("/email-verification/inspect", h.inspectEmailVerification)
	auth.POST("/email-verification/start", h.startEmailVerification)
	auth.POST("/email-verification/resend", h.resendEmailVerificationCode)
	auth.POST("/email-verification/verify", h.verifyEmailVerificationCode)
	auth.GET("/username-availability", h.usernameAvailability)
	auth.GET("/email-availability", h.emailAvailability)
	auth.POST("/login", h.login)
	auth.POST("/refresh", h.refresh)
	auth.POST("/logout", h.logout)
	auth.POST("/password-reset/inspect", h.inspectPasswordReset)
	auth.POST("/password-reset/start", h.startPasswordReset)
	auth.POST("/password-reset/resend", h.resendPasswordResetCode)
	auth.POST("/password-reset/verify", h.verifyPasswordResetCode)
	auth.POST("/password-reset/complete", h.completePasswordReset)
	auth.POST("/password-reset/claim", h.Authed(), h.claimPasswordResetForSession)
	g.GET("/me", h.Authed(), h.me)
	auth.GET("/me", h.Authed(), h.me)
	auth.POST("/password", h.Authed(), h.changePassword)
	auth.GET("/sessions", h.Authed(), h.listSessions)
	auth.DELETE("/sessions/:id", h.Authed(), h.revokeSession)

	g.PATCH("/users/me/account", h.Authed(), h.updateAccount)
	g.POST("/users/me/email-verification/inspect", h.Authed(), h.inspectEmailVerification)
	g.POST("/users/me/email-verification/start", h.Authed(), h.startEmailVerification)
	g.PATCH("/users/me/profile", h.Authed(), h.updateProfile)
	g.GET("/users/me/audio-settings", h.Authed(), h.getAudioSettings)
	g.PATCH("/users/me/audio-settings", h.Authed(), h.updateAudioSettings)
	g.DELETE("/users/me/account", h.Authed(), h.deleteAccount)
	g.GET("/users/:user_id/settings", h.Authed(), h.getForcedUserSettings)
	g.PATCH("/users/:user_id/settings", h.Authed(), h.forceUpdateUserSettings)
	g.GET("/users/:user_id/sessions", h.Authed(), h.listForcedUserSessions)
	g.DELETE("/users/:user_id/account", h.Authed(), h.forceDeleteUserAccount)
	g.GET("/users/:user_id/audio-settings", h.Authed(), h.getForcedUserAudioSettings)
	g.PATCH("/users/:user_id/audio-settings", h.Authed(), h.updateForcedUserAudioSettings)
	g.POST("/users/:user_id/password", h.Authed(), h.forceResetUserPassword)
	g.GET("/users/search", h.Authed(), h.searchUsers)
	return h
}

func (h *Handler) usernameAvailability(c *gin.Context) {
	username := strings.TrimSpace(c.Query("username"))
	if !isValidUsername(username) {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid username")
		return
	}

	taken, err := model.IsUsernameTaken(h.DB, username)
	if err != nil {
		log.Printf("auth username availability: query failed username=%q request_id=%q: %v", username, c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "username availability failed")
		return
	}
	c.JSON(http.StatusOK, AvailabilityResponse{Available: !taken})
}

func (h *Handler) emailAvailability(c *gin.Context) {
	email := strings.TrimSpace(c.Query("email"))
	if !isValidEmail(email) {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid email")
		return
	}

	taken, err := model.IsEmailTaken(h.DB, email)
	if err != nil {
		log.Printf("auth email availability: query failed email=%q request_id=%q: %v", email, c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "email availability failed")
		return
	}
	c.JSON(http.StatusOK, AvailabilityResponse{Available: !taken})
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

	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)

	if !isValidUsername(req.Username) {
		errorJSON(c, http.StatusBadRequest, "bad_request", "username must be 3-32 chars, alphanumeric/underscore/hyphen")
		return
	}
	if !isValidEmail(req.Email) {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid email")
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "password hash failed")
		return
	}

	id := uuid.New().String()
	usernameNorm := model.Normalize(req.Username)
	emailNorm := model.Normalize(req.Email)
	uid, err := idgen.NextUserUID(h.DB)
	if err != nil {
		log.Printf("auth register: allocate uid failed request_id=%q: %v", c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "create user failed")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "create user failed")
		return
	}
	defer tx.Rollback()
	if err = consumeEmailVerification(tx, emailNorm, req.EmailVerificationToken, time.Now().Unix()); errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusBadRequest, "email_verification_required", "请先验证邮箱")
		return
	} else if err != nil {
		log.Printf("auth register: verify email token failed email=%q request_id=%q: %v", req.Email, c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "email verification failed")
		return
	}
	err = model.InsertUser(tx, id, uid, req.Username, usernameNorm, req.Email, emailNorm, hash)
	if err != nil {
		if isDuplicateEntryError(err) {
			errorJSON(c, http.StatusConflict, "conflict", "username or email already taken")
			return
		}
		log.Printf("auth register: create user failed username=%q email=%q request_id=%q: %v", req.Username, req.Email, c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "create user failed")
		return
	}
	if err = tx.Commit(); err != nil {
		log.Printf("auth register: commit failed username=%q email=%q request_id=%q: %v", req.Username, req.Email, c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "create user failed")
		return
	}

	resp, err := h.issueAuthResponse(id, c)
	if err != nil {
		log.Printf("auth register: issue auth response failed user_id=%q username=%q email=%q request_id=%q: %v", id, req.Username, req.Email, c.GetString("request_id"), err)
		if cleanupErr := model.DeleteUserByID(h.DB, id); cleanupErr != nil {
			log.Printf("auth register: cleanup after failed registration failed user_id=%q request_id=%q: %v", id, c.GetString("request_id"), cleanupErr)
		}
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
	if user.Status == "suspended" {
		errorJSON(c, http.StatusForbidden, "account_suspended", "账号已被封禁")
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

	ua := c.GetHeader("User-Agent")
	ip := requestClientIP(c.Request, h.Cfg.TrustedProxies)
	if err := model.RotateRefreshToken(h.DB, session.ID, newRefresh, &ua, &ip); err != nil {
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
		_, _ = model.RevokeSession(h.DB, sid, getUserID(c))
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

	if req.CurrentPassword == "" {
		granted, err := h.hasPasswordResetGrant(getSessionID(c), user)
		if err != nil {
			errorJSON(c, http.StatusInternalServerError, "internal_error", "password reset grant check failed")
			return
		}
		if !granted {
			errorJSON(c, http.StatusUnauthorized, "password_reset_verification_required", "请先验证绑定邮箱")
			return
		}
	} else {
		ok, err := VerifyPassword(req.CurrentPassword, *user.PasswordHash)
		if err != nil || !ok {
			errorJSON(c, http.StatusUnauthorized, "unauthorized", "current password incorrect")
			return
		}
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

	ok, err := model.RevokeSession(h.DB, sessionID, getUserID(c))
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "revoke failed")
		return
	}
	if !ok {
		// No active session with that id belongs to this user; either it
		// doesn't exist or it's someone else's. Don't disclose which.
		errorJSON(c, http.StatusNotFound, "not_found", "session not found")
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
	ip := requestClientIP(c.Request, h.Cfg.TrustedProxies)
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
var emailRegex = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func isValidUsername(s string) bool {
	return usernameRegex.MatchString(s)
}

func isValidEmail(s string) bool {
	return len(s) <= 254 && emailRegex.MatchString(s)
}

func isDuplicateEntryError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "UNIQUE constraint failed") || strings.Contains(message, "Duplicate entry")
}

func errorJSON(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{
		Error: ErrorBody{Code: code, Message: apierrors.UserMessage(code, message, status), RequestID: c.GetString("request_id")},
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
	language := user.Language
	if language == "" {
		language = "zh-Hans"
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
		Language:            language,
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
