package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The console has one account, set in config.json. It is separate from the
// app's super user, whose password is in the server's source.

const (
	sessionLifetime    = 30 * 24 * time.Hour
	sessionIdleTimeout = 24 * time.Hour
	maxFailedLogins    = 8
	loginLockout       = 15 * time.Minute
	passwordMinLength  = 14
)

var commonPasswordFragments = []string{"password", "admin", "changeme", "letmein", "welcome", "qwerty"}

var (
	nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]`)
	symbolPattern   = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// passwordPolicyIssues lists what keeps password from being the console's
// password.
func passwordPolicyIssues(password, username string) []string {
	var issues []string
	if len([]rune(password)) < passwordMinLength {
		issues = append(issues, "use at least 14 characters")
	}
	if !strings.ContainsAny(password, "abcdefghijklmnopqrstuvwxyz") {
		issues = append(issues, "add a lowercase letter")
	}
	if !strings.ContainsAny(password, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		issues = append(issues, "add an uppercase letter")
	}
	if !strings.ContainsAny(password, "0123456789") {
		issues = append(issues, "add a number")
	}
	if !symbolPattern.MatchString(password) {
		issues = append(issues, "add a symbol")
	}
	lower := strings.ToLower(password)
	user := strings.ToLower(strings.TrimSpace(username))
	if compact := nonAlphanumeric.ReplaceAllString(user, ""); len(user) >= 3 &&
		(strings.Contains(lower, user) || (len(compact) >= 3 && strings.Contains(nonAlphanumeric.ReplaceAllString(lower, ""), compact))) {
		issues = append(issues, "do not include the username")
	}
	for _, fragment := range commonPasswordFragments {
		if strings.Contains(lower, fragment) {
			issues = append(issues, "avoid "+fragment)
		}
	}
	return issues
}

// loginLimiter locks the account out after repeated wrong passwords.
type loginLimiter struct {
	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
}

func (l *loginLimiter) locked(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return now.Before(l.lockedUntil)
}

func (l *loginLimiter) record(success bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if success {
		l.failures = 0
		return
	}
	l.failures++
	if l.failures >= maxFailedLogins {
		l.failures = 0
		l.lockedUntil = now.Add(loginLockout)
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func bearerToken(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// requireEnabled refuses requests while the console is off. The reason stays
// in the server log: it would tell a stranger what is wrong with the password.
func (h *Handler) requireEnabled() error {
	if h.disabledReason != "" {
		return fail(http.StatusServiceUnavailable, "ADMIN_DISABLED", "The admin console is turned off; the server log says why")
	}
	return nil
}

// status tells the sign-in page whether signing in can work.
func (h *Handler) status(*gin.Context) (any, error) {
	return gin.H{"enabled": h.disabledReason == ""}, nil
}

type sessionInfo struct {
	Username  string `json:"username"`
	ExpiresAt string `json:"expiresAt"`
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (h *Handler) login(c *gin.Context) (any, error) {
	if err := h.requireEnabled(); err != nil {
		return nil, err
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(in.Username)
	c.Set(actorKey, username)
	now := time.Now()
	if h.limiter.locked(now) {
		err := fail(http.StatusTooManyRequests, "LOGIN_LOCKED", "Too many failed sign-ins; try again in 15 minutes")
		h.audit(c, "auth.login", username, "", err)
		return nil, err
	}
	userOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(username)), []byte(strings.ToLower(h.Config.AdminUsername))) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(in.Password), []byte(h.Config.AdminPassword)) == 1
	h.limiter.record(userOK && passwordOK, now)
	if !userOK || !passwordOK {
		err := fail(http.StatusUnauthorized, "INVALID_CREDENTIALS", "Wrong username or password")
		h.audit(c, "auth.login", username, "", err)
		return nil, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := now.Add(sessionLifetime)
	if _, err := h.DB.Exec(
		`INSERT INTO admin_sessions (id, token_hash, username, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), hashToken(token), h.Config.AdminUsername, now.UnixMilli(), expires.UnixMilli(), now.UnixMilli(),
	); err != nil {
		return nil, err
	}
	// Signed-out and expired sessions are only kept until the next sign-in.
	if _, err := h.DB.Exec(`DELETE FROM admin_sessions WHERE revoked_at IS NOT NULL OR expires_at <= ? OR last_seen_at <= ?`,
		now.UnixMilli(), now.Add(-sessionIdleTimeout).UnixMilli()); err != nil {
		return nil, err
	}
	h.audit(c, "auth.login", h.Config.AdminUsername, "", nil)
	return gin.H{"token": token, "session": sessionInfo{Username: h.Config.AdminUsername, ExpiresAt: iso(expires)}}, nil
}

const actorKey = "admin_actor"

// requireSession lets a request through with a live console session, and
// keeps the session from going idle.
func (h *Handler) requireSession(c *gin.Context) {
	if err := h.requireEnabled(); err != nil {
		writeError(c, err)
		return
	}
	token := bearerToken(c)
	now := time.Now()
	var id, username string
	var expires, lastSeen int64
	err := h.DB.QueryRow(
		`SELECT id, username, expires_at, last_seen_at FROM admin_sessions
		 WHERE token_hash = ? AND revoked_at IS NULL`, hashToken(token),
	).Scan(&id, &username, &expires, &lastSeen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(c, err)
		return
	}
	if err != nil || token == "" || expires <= now.UnixMilli() || lastSeen <= now.Add(-sessionIdleTimeout).UnixMilli() {
		writeError(c, fail(http.StatusUnauthorized, "UNAUTHORIZED", "Sign in to the admin console"))
		return
	}
	// Writing on every request would compete with the chat server for the
	// write lock; once a minute is plenty for a day-long idle timeout.
	if now.UnixMilli()-lastSeen > int64(time.Minute/time.Millisecond) {
		if _, err := h.DB.Exec(`UPDATE admin_sessions SET last_seen_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
			writeError(c, err)
			return
		}
	}
	c.Set(actorKey, username)
	c.Set("admin_session_id", id)
	c.Set("admin_session_expires_at", expires)
	c.Next()
}

func (h *Handler) me(c *gin.Context) (any, error) {
	return sessionInfo{Username: c.GetString(actorKey), ExpiresAt: iso(time.UnixMilli(c.GetInt64("admin_session_expires_at")))}, nil
}

func (h *Handler) logout(c *gin.Context) (any, error) {
	_, err := h.DB.Exec(`UPDATE admin_sessions SET revoked_at = ? WHERE id = ?`, time.Now().UnixMilli(), c.GetString("admin_session_id"))
	h.audit(c, "auth.logout", c.GetString(actorKey), "", err)
	if err != nil {
		return nil, err
	}
	return gin.H{"revoked": true}, nil
}
