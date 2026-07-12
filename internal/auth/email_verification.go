package auth

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

const (
	emailVerificationCodeTTL     = 10 * time.Minute
	emailVerificationResendDelay = 60 * time.Second
	emailVerificationTokenTTL    = 10 * time.Minute
	emailVerificationMaxAttempts = 5
)

type emailVerificationChallenge struct {
	ID                string
	Email             string
	EmailNormalized   string
	CodeHash          string
	ExpiresAt         int64
	ResendAvailableAt int64
	Attempts          int
	VerifiedAt        sql.NullInt64
	ConsumedAt        sql.NullInt64
}

func (h *Handler) ensureEmailVerificationSchema() error {
	if _, err := h.DB.Exec(`CREATE TABLE IF NOT EXISTS email_verification_challenges (
		id VARCHAR(36) PRIMARY KEY NOT NULL,
		email VARCHAR(254) NOT NULL,
		email_normalized VARCHAR(254) NOT NULL,
		code_hash CHAR(64) NOT NULL,
		expires_at BIGINT NOT NULL,
		resend_available_at BIGINT NOT NULL,
		attempts INT NOT NULL DEFAULT 0,
		verified_at BIGINT NULL,
		verification_token_hash CHAR(43) NULL,
		verification_token_expires_at BIGINT NULL,
		consumed_at BIGINT NULL,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		INDEX idx_email_verification_email_created (email_normalized, created_at),
		UNIQUE INDEX idx_email_verification_token (verification_token_hash)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return err
	}
	_, err := h.DB.Exec(`CREATE TABLE IF NOT EXISTS email_verification_locks (
		email_normalized VARCHAR(254) PRIMARY KEY NOT NULL,
		updated_at BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	return err
}

func (h *Handler) inspectEmailVerification(c *gin.Context) {
	_, normalized, ok := h.availableRegistrationEmail(c)
	if !ok {
		return
	}
	if h.VerificationEmailSender == nil {
		errorJSON(c, http.StatusServiceUnavailable, "email_unavailable", "邮件发送服务尚未配置")
		return
	}
	now := time.Now().Unix()
	latest, err := latestEmailVerification(h.DB, normalized)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, EmailVerificationInspectionResponse{CanSend: true})
		return
	}
	if err != nil {
		log.Printf("email verification inspect: query failed email=%q request_id=%q: %v", normalized, c.GetString("request_id"), err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法检查邮箱验证码")
		return
	}
	retryAfter := emailVerificationRetryAfter(latest, now)
	if retryAfter <= 0 {
		c.JSON(http.StatusOK, EmailVerificationInspectionResponse{CanSend: true})
		return
	}
	var challengeID *string
	if emailVerificationChallengeCanContinue(latest, now) {
		challengeID = &latest.ID
	}
	c.JSON(http.StatusOK, EmailVerificationInspectionResponse{
		CanSend:     false,
		ChallengeID: challengeID,
		RetryAfter:  retryAfter,
	})
}

func (h *Handler) startEmailVerification(c *gin.Context) {
	email, normalized, ok := h.availableRegistrationEmail(c)
	if !ok {
		return
	}
	if h.VerificationEmailSender == nil {
		errorJSON(c, http.StatusServiceUnavailable, "email_unavailable", "邮件发送服务尚未配置")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	defer tx.Rollback()
	nowTime := time.Now()
	now := nowTime.Unix()
	if err = lockEmailVerification(tx, normalized, now); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	latest, err := latestEmailVerification(tx, normalized)
	if err == nil && emailVerificationRetryAfter(latest, now) > 0 {
		if emailVerificationChallengeCanContinue(latest, now) {
			c.JSON(http.StatusOK, emailVerificationChallengeResponse(latest, now))
			return
		}
		errorJSON(c, http.StatusTooManyRequests, "email_verification_cooldown", fmt.Sprintf("验证码已发送，请在 %d 秒后重试", emailVerificationRetryAfter(latest, now)))
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	challenge, code, err := h.newEmailVerificationChallenge(email, normalized, nowTime)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法生成验证码")
		return
	}
	_, err = tx.Exec(
		`INSERT INTO email_verification_challenges
		 (id, email, email_normalized, code_hash, expires_at, resend_available_at, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		challenge.ID, challenge.Email, challenge.EmailNormalized, challenge.CodeHash,
		challenge.ExpiresAt, challenge.ResendAvailableAt, now, now,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存验证码")
		return
	}
	if err = h.VerificationEmailSender.SendRegistrationVerificationCode(c.Request.Context(), email, code); err != nil {
		log.Printf("email verification start: send failed email=%q request_id=%q: %v", normalized, c.GetString("request_id"), err)
		errorJSON(c, http.StatusBadGateway, "email_send_failed", "验证码邮件发送失败，请稍后重试")
		return
	}
	if _, err = tx.Exec(
		`UPDATE email_verification_challenges SET consumed_at = ?, updated_at = ?
		 WHERE email_normalized = ? AND id != ? AND consumed_at IS NULL`,
		now, now, normalized, challenge.ID,
	); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存验证码")
		return
	}
	if err = tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存验证码")
		return
	}
	c.JSON(http.StatusOK, emailVerificationChallengeResponse(challenge, now))
}

func (h *Handler) resendEmailVerificationCode(c *gin.Context) {
	var req EmailVerificationChallengeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "邮箱验证请求无效")
		return
	}
	initial, err := emailVerificationByID(h.DB, strings.TrimSpace(req.ChallengeID), false)
	if errors.Is(err, sql.ErrNoRows) {
		errorJSON(c, http.StatusGone, "verification_expired", "邮箱验证已失效，请重新验证")
		return
	}
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	if h.VerificationEmailSender == nil {
		errorJSON(c, http.StatusServiceUnavailable, "email_unavailable", "邮件发送服务尚未配置")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	defer tx.Rollback()
	nowTime := time.Now()
	now := nowTime.Unix()
	if err = lockEmailVerification(tx, initial.EmailNormalized, now); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法发送验证码")
		return
	}
	challenge, err := emailVerificationByID(tx, initial.ID, true)
	if err != nil || !emailVerificationChallengeCanContinue(challenge, now) {
		errorJSON(c, http.StatusGone, "verification_expired", "邮箱验证已失效，请重新验证")
		return
	}
	if retryAfter := emailVerificationRetryAfter(challenge, now); retryAfter > 0 {
		errorJSON(c, http.StatusTooManyRequests, "email_verification_cooldown", fmt.Sprintf("验证码已发送，请在 %d 秒后重试", retryAfter))
		return
	}
	code, err := randomNumericCode()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法生成验证码")
		return
	}
	challenge.CodeHash = h.passwordResetCodeHash(challenge.ID, code)
	challenge.ExpiresAt = nowTime.Add(emailVerificationCodeTTL).Unix()
	challenge.ResendAvailableAt = nowTime.Add(emailVerificationResendDelay).Unix()
	if err = h.VerificationEmailSender.SendRegistrationVerificationCode(c.Request.Context(), challenge.Email, code); err != nil {
		log.Printf("email verification resend: send failed email=%q request_id=%q: %v", challenge.EmailNormalized, c.GetString("request_id"), err)
		errorJSON(c, http.StatusBadGateway, "email_send_failed", "验证码邮件发送失败，请稍后重试")
		return
	}
	_, err = tx.Exec(
		`UPDATE email_verification_challenges
		 SET code_hash = ?, expires_at = ?, resend_available_at = ?, attempts = 0, updated_at = ?
		 WHERE id = ?`,
		challenge.CodeHash, challenge.ExpiresAt, challenge.ResendAvailableAt, now, challenge.ID,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存验证码")
		return
	}
	if err = tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法保存验证码")
		return
	}
	c.JSON(http.StatusOK, emailVerificationChallengeResponse(challenge, now))
}

func (h *Handler) verifyEmailVerificationCode(c *gin.Context) {
	var req VerifyEmailVerificationRequest
	if err := c.ShouldBindJSON(&req); err != nil || !passwordResetCodePattern.MatchString(req.Code) {
		errorJSON(c, http.StatusBadRequest, "bad_request", "请输入 6 位数字验证码")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证邮箱")
		return
	}
	defer tx.Rollback()
	challenge, err := emailVerificationByID(tx, strings.TrimSpace(req.ChallengeID), true)
	now := time.Now().Unix()
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !emailVerificationChallengeCanContinue(challenge, now)) {
		errorJSON(c, http.StatusGone, "verification_expired", "邮箱验证已失效，请重新验证")
		return
	}
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证邮箱")
		return
	}
	expected := h.passwordResetCodeHash(challenge.ID, req.Code)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(challenge.CodeHash)) != 1 {
		attempts := challenge.Attempts + 1
		var consumedAt any
		if attempts >= emailVerificationMaxAttempts {
			consumedAt = now
		}
		_, err = tx.Exec(
			`UPDATE email_verification_challenges SET attempts = ?, consumed_at = ?, updated_at = ? WHERE id = ?`,
			attempts, consumedAt, now, challenge.ID,
		)
		if err != nil || tx.Commit() != nil {
			errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证邮箱")
			return
		}
		errorJSON(c, http.StatusBadRequest, "invalid_verification_code", "验证码错误")
		return
	}
	token, err := randomURLToken(32)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证邮箱")
		return
	}
	_, err = tx.Exec(
		`UPDATE email_verification_challenges
		 SET verified_at = ?, verification_token_hash = ?, verification_token_expires_at = ?, updated_at = ?
		 WHERE id = ?`,
		now, hashResetToken(token), time.Now().Add(emailVerificationTokenTTL).Unix(), now, challenge.ID,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证邮箱")
		return
	}
	if err = tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法验证邮箱")
		return
	}
	c.JSON(http.StatusOK, EmailVerificationResponse{VerificationToken: token})
}

func (h *Handler) availableRegistrationEmail(c *gin.Context) (string, string, bool) {
	var req EmailVerificationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "请输入有效的邮箱地址")
		return "", "", false
	}
	email := strings.TrimSpace(req.Email)
	if !isValidEmail(email) {
		errorJSON(c, http.StatusBadRequest, "bad_request", "请输入有效的邮箱地址")
		return "", "", false
	}
	taken, err := model.IsEmailTaken(h.DB, email)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "暂时无法检测邮箱是否重复")
		return "", "", false
	}
	if taken {
		errorJSON(c, http.StatusConflict, "conflict", "该邮箱已被其他用户使用")
		return "", "", false
	}
	return email, model.Normalize(email), true
}

func lockEmailVerification(tx *sql.Tx, normalized string, now int64) error {
	if _, err := tx.Exec(
		`INSERT INTO email_verification_locks (email_normalized, updated_at) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE updated_at = VALUES(updated_at)`,
		normalized, now,
	); err != nil {
		return err
	}
	var locked string
	return tx.QueryRow(
		`SELECT email_normalized FROM email_verification_locks WHERE email_normalized = ? FOR UPDATE`,
		normalized,
	).Scan(&locked)
}

func latestEmailVerification(queryer passwordResetQueryer, normalized string) (*emailVerificationChallenge, error) {
	challenge := &emailVerificationChallenge{}
	err := queryer.QueryRow(
		`SELECT id, email, email_normalized, code_hash, expires_at, resend_available_at, attempts, verified_at, consumed_at
		 FROM email_verification_challenges WHERE email_normalized = ?
		 ORDER BY created_at DESC, updated_at DESC LIMIT 1`,
		normalized,
	).Scan(
		&challenge.ID, &challenge.Email, &challenge.EmailNormalized, &challenge.CodeHash,
		&challenge.ExpiresAt, &challenge.ResendAvailableAt, &challenge.Attempts,
		&challenge.VerifiedAt, &challenge.ConsumedAt,
	)
	return challenge, err
}

func emailVerificationByID(queryer passwordResetQueryer, id string, forUpdate bool) (*emailVerificationChallenge, error) {
	challenge := &emailVerificationChallenge{}
	query := `SELECT id, email, email_normalized, code_hash, expires_at, resend_available_at, attempts, verified_at, consumed_at
		 FROM email_verification_challenges WHERE id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	err := queryer.QueryRow(query, id).Scan(
		&challenge.ID, &challenge.Email, &challenge.EmailNormalized, &challenge.CodeHash,
		&challenge.ExpiresAt, &challenge.ResendAvailableAt, &challenge.Attempts,
		&challenge.VerifiedAt, &challenge.ConsumedAt,
	)
	return challenge, err
}

func (h *Handler) newEmailVerificationChallenge(email, normalized string, now time.Time) (*emailVerificationChallenge, string, error) {
	code, err := randomNumericCode()
	if err != nil {
		return nil, "", err
	}
	challenge := &emailVerificationChallenge{
		ID:                uuid.NewString(),
		Email:             email,
		EmailNormalized:   normalized,
		ExpiresAt:         now.Add(emailVerificationCodeTTL).Unix(),
		ResendAvailableAt: now.Add(emailVerificationResendDelay).Unix(),
	}
	challenge.CodeHash = h.passwordResetCodeHash(challenge.ID, code)
	return challenge, code, nil
}

func emailVerificationChallengeCanContinue(challenge *emailVerificationChallenge, now int64) bool {
	return challenge != nil && !challenge.ConsumedAt.Valid && !challenge.VerifiedAt.Valid &&
		challenge.ExpiresAt > now && challenge.Attempts < emailVerificationMaxAttempts
}

func emailVerificationRetryAfter(challenge *emailVerificationChallenge, now int64) int64 {
	if challenge == nil || challenge.ResendAvailableAt <= now {
		return 0
	}
	return challenge.ResendAvailableAt - now
}

func emailVerificationChallengeResponse(challenge *emailVerificationChallenge, now int64) EmailVerificationChallengeResponse {
	return EmailVerificationChallengeResponse{
		ChallengeID: challenge.ID,
		RetryAfter:  emailVerificationRetryAfter(challenge, now),
	}
}

func consumeEmailVerification(tx *sql.Tx, emailNormalized, token string, now int64) error {
	var challengeID string
	err := tx.QueryRow(
		`SELECT id FROM email_verification_challenges
		 WHERE email_normalized = ? AND verification_token_hash = ? AND verified_at IS NOT NULL
		 AND consumed_at IS NULL AND verification_token_expires_at > ? FOR UPDATE`,
		emailNormalized, hashResetToken(strings.TrimSpace(token)), now,
	).Scan(&challengeID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`UPDATE email_verification_challenges SET consumed_at = ?, updated_at = ? WHERE id = ?`,
		now, now, challengeID,
	)
	return err
}
