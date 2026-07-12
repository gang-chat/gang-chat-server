package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

const (
	passwordResetCodeTTL     = 10 * time.Minute
	passwordResetResendDelay = 59 * time.Second
	passwordResetTokenTTL    = 10 * time.Minute
	passwordResetMaxAttempts = 5
)

var passwordResetCodePattern = regexp.MustCompile(`^[0-9]{6}$`)

type passwordResetChallenge struct {
	ID                string
	UserID            string
	Email             string
	CodeHash          string
	ExpiresAt         int64
	ResendAvailableAt int64
	Attempts          int
	VerifiedAt        sql.NullInt64
	ConsumedAt        sql.NullInt64
}

func (h *Handler) ensurePasswordResetSchema() error {
	if _, err := h.DB.Exec(`CREATE TABLE IF NOT EXISTS password_reset_challenges (
		id VARCHAR(36) PRIMARY KEY NOT NULL,
		user_id VARCHAR(128) NOT NULL,
		email VARCHAR(320) NOT NULL,
		code_hash CHAR(64) NOT NULL,
		expires_at BIGINT NOT NULL,
		resend_available_at BIGINT NOT NULL,
		attempts INT NOT NULL DEFAULT 0,
		verified_at BIGINT NULL,
		reset_token_hash CHAR(43) NULL,
		reset_token_expires_at BIGINT NULL,
		consumed_at BIGINT NULL,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		INDEX idx_password_reset_user_created (user_id, created_at),
		UNIQUE INDEX idx_password_reset_token (reset_token_hash)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return err
	}
	_, err := h.DB.Exec(`CREATE TABLE IF NOT EXISTS password_reset_session_grants (
		session_id VARCHAR(128) PRIMARY KEY NOT NULL,
		user_id VARCHAR(128) NOT NULL,
		email_normalized VARCHAR(320) NOT NULL,
		expires_at BIGINT NOT NULL,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		INDEX idx_password_reset_grant_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	return err
}

func (h *Handler) startPasswordReset(c *gin.Context) {
	var req StartPasswordResetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "请输入用户名或邮箱")
		return
	}
	login := strings.TrimSpace(req.Login)
	user, err := model.GetUserByUsernameOrEmail(h.DB, login)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (user.Status != "active" || user.DeletedAt.Valid)) {
		errorJSON(c, http.StatusNotFound, "account_not_found", "该用户名或邮箱对应的账号不存在")
		return
	}
	if err != nil {
		log.Printf("password reset start: lookup failed request_id=%q: %v", c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法检查账号")
		return
	}
	if h.PasswordResetEmailSender == nil {
		errorJSON(c, http.StatusServiceUnavailable, "email_unavailable", "邮件发送服务尚未配置")
		return
	}

	now := time.Now().Unix()
	if existing, err := h.latestActivePasswordReset(user.ID); err == nil && existing.ExpiresAt > now && existing.ResendAvailableAt > now {
		c.JSON(http.StatusOK, passwordResetChallengeResponse(existing, now))
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("password reset start: existing challenge query failed user_id=%q: %v", user.ID, err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}

	challenge, code, err := h.createPasswordResetChallenge(user)
	if err != nil {
		log.Printf("password reset start: challenge create failed user_id=%q: %v", user.ID, err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	if err := h.PasswordResetEmailSender.SendPasswordResetCode(c.Request.Context(), user.Email, code); err != nil {
		_, _ = h.DB.Exec(`DELETE FROM password_reset_challenges WHERE id = ?`, challenge.ID)
		log.Printf("password reset start: email failed user_id=%q request_id=%q: %v", user.ID, c.GetString("request_id"), err)
		errorJSON(c, http.StatusServiceUnavailable, "email_send_failed", "验证码发送失败，请稍后重试")
		return
	}
	_, _ = h.DB.Exec(
		`UPDATE password_reset_challenges SET consumed_at = ?, updated_at = ? WHERE user_id = ? AND id != ? AND consumed_at IS NULL`,
		now, now, user.ID, challenge.ID,
	)
	c.JSON(http.StatusOK, passwordResetChallengeResponse(challenge, now))
}

func (h *Handler) resendPasswordResetCode(c *gin.Context) {
	var req PasswordResetChallengeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "无效的验证码请求")
		return
	}
	challenge, err := h.getPasswordResetChallenge(strings.TrimSpace(req.ChallengeID))
	if errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusNotFound, "challenge_not_found", "验证码请求不存在")
		return
	}
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重新发送验证码")
		return
	}
	now := time.Now().Unix()
	if challenge.ConsumedAt.Valid || challenge.VerifiedAt.Valid || challenge.ExpiresAt <= now {
		errorJSON(c, http.StatusGone, "challenge_expired", "验证码已失效，请重新发起密码重置")
		return
	}
	if challenge.ResendAvailableAt > now {
		c.JSON(http.StatusOK, passwordResetChallengeResponse(challenge, now))
		return
	}
	if h.PasswordResetEmailSender == nil {
		errorJSON(c, http.StatusServiceUnavailable, "email_unavailable", "邮件发送服务尚未配置")
		return
	}
	code, err := randomNumericCode()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重新发送验证码")
		return
	}
	challenge.CodeHash = h.passwordResetCodeHash(challenge.ID, code)
	challenge.ExpiresAt = time.Now().Add(passwordResetCodeTTL).Unix()
	challenge.ResendAvailableAt = time.Now().Add(passwordResetResendDelay).Unix()
	res, err := h.DB.Exec(
		`UPDATE password_reset_challenges SET code_hash = ?, expires_at = ?, resend_available_at = ?, attempts = 0, updated_at = ?
		 WHERE id = ? AND consumed_at IS NULL AND verified_at IS NULL`,
		challenge.CodeHash, challenge.ExpiresAt, challenge.ResendAvailableAt, now, challenge.ID,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重新发送验证码")
		return
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		errorJSON(c, http.StatusGone, "challenge_expired", "验证码已失效，请重新发起密码重置")
		return
	}
	if err := h.PasswordResetEmailSender.SendPasswordResetCode(c.Request.Context(), challenge.Email, code); err != nil {
		_, _ = h.DB.Exec(`UPDATE password_reset_challenges SET consumed_at = ?, updated_at = ? WHERE id = ?`, now, now, challenge.ID)
		log.Printf("password reset resend: email failed challenge_id=%q: %v", challenge.ID, err)
		errorJSON(c, http.StatusServiceUnavailable, "email_send_failed", "验证码发送失败，请重新发起密码重置")
		return
	}
	c.JSON(http.StatusOK, passwordResetChallengeResponse(challenge, now))
}

func (h *Handler) verifyPasswordResetCode(c *gin.Context) {
	var req VerifyPasswordResetRequest
	if err := c.ShouldBindJSON(&req); err != nil || !passwordResetCodePattern.MatchString(req.Code) {
		errorJSON(c, http.StatusBadRequest, "invalid_code", "请输入 6 位数字验证码")
		return
	}
	challenge, err := h.getPasswordResetChallenge(strings.TrimSpace(req.ChallengeID))
	if errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusNotFound, "challenge_not_found", "验证码请求不存在")
		return
	}
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证验证码")
		return
	}
	now := time.Now().Unix()
	if challenge.ConsumedAt.Valid || challenge.VerifiedAt.Valid || challenge.ExpiresAt <= now {
		errorJSON(c, http.StatusGone, "challenge_expired", "验证码已失效，请重新发起密码重置")
		return
	}
	if challenge.Attempts >= passwordResetMaxAttempts {
		errorJSON(c, http.StatusTooManyRequests, "too_many_attempts", "验证码错误次数过多，请重新发起密码重置")
		return
	}
	expected := h.passwordResetCodeHash(challenge.ID, req.Code)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(challenge.CodeHash)) != 1 {
		attempts := challenge.Attempts + 1
		consumedAt := any(nil)
		if attempts >= passwordResetMaxAttempts {
			consumedAt = now
		}
		_, _ = h.DB.Exec(`UPDATE password_reset_challenges SET attempts = ?, consumed_at = ?, updated_at = ? WHERE id = ?`, attempts, consumedAt, now, challenge.ID)
		if attempts >= passwordResetMaxAttempts {
			errorJSON(c, http.StatusTooManyRequests, "too_many_attempts", "验证码错误次数过多，请重新发起密码重置")
			return
		}
		errorJSON(c, http.StatusBadRequest, "invalid_code", "验证码错误")
		return
	}
	resetToken, err := randomURLToken(32)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证验证码")
		return
	}
	res, err := h.DB.Exec(
		`UPDATE password_reset_challenges SET verified_at = ?, reset_token_hash = ?, reset_token_expires_at = ?, updated_at = ?
		 WHERE id = ? AND consumed_at IS NULL AND verified_at IS NULL`,
		now, hashResetToken(resetToken), time.Now().Add(passwordResetTokenTTL).Unix(), now, challenge.ID,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证验证码")
		return
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		errorJSON(c, http.StatusGone, "challenge_expired", "验证码已失效，请重新发起密码重置")
		return
	}
	c.JSON(http.StatusOK, PasswordResetVerificationResponse{ResetToken: resetToken})
}

func (h *Handler) completePasswordReset(c *gin.Context) {
	var req CompletePasswordResetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "新密码至少需要 8 个字符")
		return
	}
	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "密码加密失败")
		return
	}
	now := time.Now().Unix()
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重置密码")
		return
	}
	defer tx.Rollback()
	var challengeID, userID string
	err = tx.QueryRow(
		`SELECT id, user_id FROM password_reset_challenges
		 WHERE reset_token_hash = ? AND verified_at IS NOT NULL AND consumed_at IS NULL AND reset_token_expires_at > ? FOR UPDATE`,
		hashResetToken(req.ResetToken), now,
	).Scan(&challengeID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusGone, "reset_token_expired", "密码重置凭证已失效，请重新验证邮箱")
		return
	}
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重置密码")
		return
	}
	if _, err = tx.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ? AND status = 'active'`, newHash, now, userID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重置密码")
		return
	}
	if _, err = tx.Exec(`UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重置密码")
		return
	}
	if _, err = tx.Exec(`UPDATE password_reset_challenges SET consumed_at = ?, updated_at = ? WHERE user_id = ? AND consumed_at IS NULL`, now, now, userID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重置密码")
		return
	}
	if err = tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法重置密码")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) claimPasswordResetForSession(c *gin.Context) {
	var req ClaimPasswordResetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "密码重置凭证不能为空")
		return
	}
	now := time.Now().Unix()
	userID := getUserID(c)
	sessionID := getSessionID(c)
	session, err := model.GetSessionByID(h.DB, sessionID)
	if err != nil || session.UserID != userID || session.RevokedAt != nil || session.ExpiresAt <= now {
		errorJSON(c, http.StatusUnauthorized, "unauthorized", "当前登录会话已失效")
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存邮箱验证状态")
		return
	}
	defer tx.Rollback()
	var challengeID string
	err = tx.QueryRow(
		`SELECT id FROM password_reset_challenges
		 WHERE reset_token_hash = ? AND user_id = ? AND verified_at IS NOT NULL
		 AND consumed_at IS NULL AND reset_token_expires_at > ? FOR UPDATE`,
		hashResetToken(req.ResetToken), userID, now,
	).Scan(&challengeID)
	if errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusGone, "reset_token_expired", "邮箱验证凭证已失效，请重新验证")
		return
	}
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存邮箱验证状态")
		return
	}
	user, err := model.GetUserByID(h.DB, userID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存邮箱验证状态")
		return
	}
	_, err = tx.Exec(
		`INSERT INTO password_reset_session_grants
		 (session_id, user_id, email_normalized, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE user_id = VALUES(user_id), email_normalized = VALUES(email_normalized),
		 expires_at = VALUES(expires_at), updated_at = VALUES(updated_at)`,
		sessionID, userID, user.EmailNormalized, session.ExpiresAt, now, now,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存邮箱验证状态")
		return
	}
	if _, err = tx.Exec(`UPDATE password_reset_challenges SET consumed_at = ?, updated_at = ? WHERE id = ?`, now, now, challengeID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存邮箱验证状态")
		return
	}
	if err = tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存邮箱验证状态")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) hasPasswordResetGrant(sessionID string, user *model.User) (bool, error) {
	var granted int
	err := h.DB.QueryRow(
		`SELECT EXISTS(
		 SELECT 1 FROM password_reset_session_grants
		 WHERE session_id = ? AND user_id = ? AND email_normalized = ? AND expires_at > ?
		)`,
		sessionID, user.ID, user.EmailNormalized, time.Now().Unix(),
	).Scan(&granted)
	return granted != 0, err
}

func (h *Handler) latestActivePasswordReset(userID string) (*passwordResetChallenge, error) {
	challenge := &passwordResetChallenge{}
	err := h.DB.QueryRow(
		`SELECT id, user_id, email, code_hash, expires_at, resend_available_at, attempts, verified_at, consumed_at
		 FROM password_reset_challenges WHERE user_id = ? AND consumed_at IS NULL AND verified_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`,
		userID,
	).Scan(&challenge.ID, &challenge.UserID, &challenge.Email, &challenge.CodeHash, &challenge.ExpiresAt, &challenge.ResendAvailableAt, &challenge.Attempts, &challenge.VerifiedAt, &challenge.ConsumedAt)
	return challenge, err
}

func (h *Handler) getPasswordResetChallenge(id string) (*passwordResetChallenge, error) {
	challenge := &passwordResetChallenge{}
	err := h.DB.QueryRow(
		`SELECT id, user_id, email, code_hash, expires_at, resend_available_at, attempts, verified_at, consumed_at
		 FROM password_reset_challenges WHERE id = ?`, id,
	).Scan(&challenge.ID, &challenge.UserID, &challenge.Email, &challenge.CodeHash, &challenge.ExpiresAt, &challenge.ResendAvailableAt, &challenge.Attempts, &challenge.VerifiedAt, &challenge.ConsumedAt)
	return challenge, err
}

func (h *Handler) createPasswordResetChallenge(user *model.User) (*passwordResetChallenge, string, error) {
	code, err := randomNumericCode()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	challenge := &passwordResetChallenge{
		ID:                uuid.NewString(),
		UserID:            user.ID,
		Email:             user.Email,
		ExpiresAt:         now.Add(passwordResetCodeTTL).Unix(),
		ResendAvailableAt: now.Add(passwordResetResendDelay).Unix(),
	}
	challenge.CodeHash = h.passwordResetCodeHash(challenge.ID, code)
	_, err = h.DB.Exec(
		`INSERT INTO password_reset_challenges
		 (id, user_id, email, code_hash, expires_at, resend_available_at, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		challenge.ID, challenge.UserID, challenge.Email, challenge.CodeHash, challenge.ExpiresAt, challenge.ResendAvailableAt, now.Unix(), now.Unix(),
	)
	if err != nil {
		return nil, "", err
	}
	return challenge, code, nil
}

func (h *Handler) passwordResetCodeHash(challengeID, code string) string {
	mac := hmac.New(sha256.New, []byte(h.Cfg.JWTSecret))
	_, _ = fmt.Fprintf(mac, "%s\x00%s", challengeID, code)
	return hex.EncodeToString(mac.Sum(nil))
}

func passwordResetChallengeResponse(challenge *passwordResetChallenge, now int64) PasswordResetChallengeResponse {
	retryAfter := challenge.ResendAvailableAt - now
	if retryAfter < 0 {
		retryAfter = 0
	}
	return PasswordResetChallengeResponse{
		ChallengeID: challenge.ID,
		MaskedEmail: maskEmail(challenge.Email),
		RetryAfter:  retryAfter,
	}
}

func randomNumericCode() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	value := (uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])) % 1000000
	return fmt.Sprintf("%06d", value), nil
}

func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashResetToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func maskEmail(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return "***"
	}
	local := []rune(parts[0])
	visible := ""
	if len(local) > 0 {
		visible = string(local[0])
	}
	if len(local) > 2 {
		visible += string(local[len(local)-1])
	}
	return visible + "***@" + parts[1]
}
