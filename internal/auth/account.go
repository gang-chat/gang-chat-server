package auth

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

func (h *Handler) updateAccount(c *gin.Context) {
	var req UpdateAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Username == nil && req.Email == nil && req.EmailPublic == nil &&
		req.PhoneNumber == nil && req.PhoneNumberPublic == nil &&
		req.Language == nil {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "at least one account field is required")
		return
	}

	user, ok := getUserFromContext(c, h.DB)
	if !ok {
		return
	}

	now := time.Now().Unix()
	sets := []string{"updated_at = ?"}
	args := []any{now}
	var verificationEmailNormalized string
	var verificationToken string

	if req.Username != nil {
		username := strings.TrimSpace(*req.Username)
		if !isValidUsername(username) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "username must be 3-32 chars, alphanumeric/underscore/hyphen")
			return
		}
		lastChanged := user.UsernameUpdatedAt.Int64
		if user.UsernameUpdatedAt.Valid && lastChanged > user.CreatedAt && now-lastChanged < 24*60*60 {
			c.Header("Retry-After", strconvItoa(24*60*60-(now-lastChanged)))
			errorJSON(c, http.StatusTooManyRequests, "rate_limited", "username can be changed once per 24 hours")
			return
		}
		sets = append(sets, "username = ?", "username_normalized = ?", "username_updated_at = ?")
		args = append(args, username, strings.ToLower(username), now)
	}

	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if !isValidEmail(email) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid email")
			return
		}
		emailNormalized := strings.ToLower(email)
		sets = append(sets, "email = ?", "email_normalized = ?")
		args = append(args, email, emailNormalized)
		if emailNormalized != user.EmailNormalized || !user.EmailVerified {
			if req.EmailVerificationToken == nil || strings.TrimSpace(*req.EmailVerificationToken) == "" {
				errorJSON(c, http.StatusBadRequest, "email_verification_required", "请先验证邮箱")
				return
			}
			verificationEmailNormalized = emailNormalized
			verificationToken = strings.TrimSpace(*req.EmailVerificationToken)
			sets = append(sets, "email_verified = 1")
		}
	}

	if req.EmailPublic != nil {
		sets = append(sets, "email_public = ?")
		args = append(args, *req.EmailPublic)
	}

	if req.PhoneNumber != nil {
		phone := strings.TrimSpace(*req.PhoneNumber)
		if phone == "" {
			sets = append(sets, "phone_number = NULL", "phone_number_normalized = NULL")
		} else {
			normalized, ok := normalizePhoneNumber(phone)
			if !ok {
				errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid phone number")
				return
			}
			sets = append(sets, "phone_number = ?", "phone_number_normalized = ?")
			args = append(args, phone, normalized)
		}
	}

	if req.PhoneNumberPublic != nil {
		sets = append(sets, "phone_number_public = ?")
		args = append(args, *req.PhoneNumberPublic)
	}

	if req.Language != nil {
		language := strings.TrimSpace(*req.Language)
		if !isValidLanguage(language) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "language must be zh-Hans, zh-Hant or en")
			return
		}
		sets = append(sets, "language = ?")
		args = append(args, language)
	}

	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "account update failed")
		return
	}
	defer tx.Rollback()
	if verificationEmailNormalized != "" {
		err = consumeEmailVerification(
			tx,
			verificationEmailNormalized,
			verificationToken,
			time.Now().Unix(),
		)
		if errors.Is(err, sql.ErrNoRows) {
			errorJSON(c, http.StatusBadRequest, "email_verification_required", "请先验证邮箱")
			return
		}
		if err != nil {
			log.Printf(
				"auth account update: verify email token failed user_id=%q email=%q request_id=%q: %v",
				user.ID,
				verificationEmailNormalized,
				c.GetString("request_id"),
				err,
			)
			errorJSON(c, http.StatusInternalServerError, "internal_error", "email verification failed")
			return
		}
	}

	args = append(args, user.ID)
	_, err = tx.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			errorJSON(c, http.StatusConflict, "conflict", "username, email or phone number already taken")
			return
		}
		errorJSON(c, http.StatusInternalServerError, "internal_error", "account update failed")
		return
	}
	if err = tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "account update failed")
		return
	}
	updated, err := model.GetUserByID(h.DB, user.ID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userResponse(updated)})
}

func (h *Handler) updateProfile(c *gin.Context) {
	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.DisplayName == nil && req.Bio == nil && req.Gender == nil &&
		req.AvatarAssetID == nil && req.DefaultAvatarKey == nil {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "at least one profile field is required")
		return
	}

	user, ok := getUserFromContext(c, h.DB)
	if !ok {
		return
	}

	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Unix()}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" || utf8.RuneCountInString(name) > 32 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "display_name must be 1-32 characters")
			return
		}
		sets = append(sets, "display_name = ?")
		args = append(args, name)
	}
	if req.Bio != nil {
		bio := strings.TrimSpace(*req.Bio)
		if len([]rune(bio)) > 500 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "bio must be at most 500 characters")
			return
		}
		sets = append(sets, "bio = ?")
		args = append(args, bio)
	}
	if req.Gender != nil {
		gender := strings.TrimSpace(*req.Gender)
		if gender != "male" && gender != "female" && gender != "secret" {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "gender must be male, female or secret")
			return
		}
		sets = append(sets, "gender = ?")
		args = append(args, gender)
	}
	if req.AvatarAssetID != nil {
		assetID := strings.TrimSpace(*req.AvatarAssetID)
		var filename sql.NullString
		if assetID != "" {
			_ = h.DB.QueryRow(`SELECT filename FROM assets WHERE id = ? AND owner_user_id = ?`, assetID, user.ID).Scan(&filename)
		}
		sets = append(sets, "avatar_url = ?")
		if filename.Valid {
			args = append(args, assetURL(assetID, filename.String))
		} else {
			args = append(args, nil)
		}
	}
	if req.DefaultAvatarKey != nil {
		key := strings.TrimSpace(*req.DefaultAvatarKey)
		if !isValidAvatarKey(key) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid default avatar key")
			return
		}
		sets = append(sets, "default_avatar_key = ?")
		args = append(args, key)
	}
	args = append(args, user.ID)
	if _, err := h.DB.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "profile update failed")
		return
	}
	updated, err := model.GetUserByID(h.DB, user.ID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userResponse(updated)})
}

func (h *Handler) searchUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "user search keyword is required")
		return
	}
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if n, err := parseSmallPositiveInt(raw, 50); err == nil {
			limit = n
		}
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid user search cursor")
			return
		}
		offset = n
	}
	matchArgs := []any{q, strings.ToLower(q), q, q}
	statusPredicate := "status = 'active'"
	if c.Query("include_suspended") == "true" && model.IsSuperuser(h.DB, getUserID(c)) {
		statusPredicate = "status IN ('active', 'suspended')"
	}
	var totalCount int
	if err := h.DB.QueryRow(
		`SELECT COUNT(*)
		 FROM users
		 WHERE `+statusPredicate+`
		   AND (
		     uid = ?
		     OR username_normalized = ?
		     OR instr(username_normalized, lower(?)) > 0
		     OR instr(lower(COALESCE(display_name, username)), lower(?)) > 0
		   )`,
		matchArgs...,
	).Scan(&totalCount); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "user search failed")
		return
	}
	rows, err := h.DB.Query(
		`SELECT id, uid, username, display_name, avatar_url, default_avatar_key, is_superuser
		 FROM users
		 WHERE `+statusPredicate+`
		   AND (
		     uid = ?
		     OR username_normalized = ?
		     OR instr(username_normalized, lower(?)) > 0
		     OR instr(lower(COALESCE(display_name, username)), lower(?)) > 0
		   )
		 ORDER BY
		   CASE
		     WHEN uid = ? THEN 0
		     WHEN username_normalized = ? THEN 1
		     WHEN instr(username_normalized, lower(?)) > 0 THEN 2
		     ELSE 3
		   END,
		   username ASC
		 LIMIT ? OFFSET ?`,
		q, strings.ToLower(q), q, q, q, strings.ToLower(q), q, limit, offset,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "user search failed")
		return
	}
	defer rows.Close()

	users := make([]gin.H, 0)
	for rows.Next() {
		var id, uid, username string
		var displayName, avatarURL, defaultAvatar sql.NullString
		var isSuperuser int
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &isSuperuser); err != nil {
			errorJSON(c, http.StatusInternalServerError, "internal_error", "read user failed")
			return
		}
		users = append(users, gin.H{
			"id": id, "uid": uid, "username": username,
			"display_name":       nullableOr(displayName, username),
			"avatar_url":         nullablePtrString(avatarURL),
			"default_avatar_key": nullableOr(defaultAvatar, "blue-3"),
			"is_superuser":       isSuperuser != 0,
		})
	}
	if err := rows.Err(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "read user failed")
		return
	}
	var nextCursor any
	if nextOffset := offset + len(users); nextOffset < totalCount {
		nextCursor = strconv.Itoa(nextOffset)
	}
	c.JSON(http.StatusOK, gin.H{
		"users": users, "next_cursor": nextCursor, "total_count": totalCount,
	})
}

type deleteAccountRequest struct {
	Confirm bool `json:"confirm"`
}

func (h *Handler) deleteAccount(c *gin.Context) {
	var req deleteAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil || !req.Confirm {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "confirm must be true")
		return
	}
	user, ok := getUserFromContext(c, h.DB)
	if !ok {
		return
	}
	if user.IsSuperuser {
		errorJSON(c, http.StatusForbidden, "forbidden", "super user account cannot be deleted")
		return
	}
	// Account deletion must fail closed if the retention migration cannot be
	// applied; otherwise legacy CASCADE constraints could erase message history
	// and the assets referenced by it.
	if err := model.EnsureHistoricalMessageRetentionSchema(h.DB); err != nil {
		log.Printf("auth: prepare historical message retention: %v", err)
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	defer tx.Rollback()
	if err := model.BackfillMessageSenderSnapshots(tx, user.ID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	// Explicitly detach assets before deleting the user. This protects retained
	// message attachments even if the database has an older FK name.
	if _, err := tx.Exec(`UPDATE assets SET owner_user_id = NULL WHERE owner_user_id = ?`, user.ID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, user.ID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	if err := tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

type forceUserSettingsRequest struct {
	Username          *string `json:"username"`
	Email             *string `json:"email"`
	EmailVerified     *bool   `json:"email_verified"`
	EmailPublic       *bool   `json:"email_public"`
	PhoneNumber       *string `json:"phone_number"`
	PhoneNumberPublic *bool   `json:"phone_number_public"`
	DisplayName       *string `json:"display_name"`
	Bio               *string `json:"bio"`
	Gender            *string `json:"gender"`
	AvatarAssetID     *string `json:"avatar_asset_id"`
	DefaultAvatarKey  *string `json:"default_avatar_key"`
	Language          *string `json:"language"`
	Status            *string `json:"status"`
}

func (h *Handler) forceUpdateUserSettings(c *gin.Context) {
	if !model.IsSuperuser(h.DB, getUserID(c)) {
		errorJSON(c, http.StatusForbidden, "forbidden", "super user required")
		return
	}
	var req forceUserSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Username == nil && req.Email == nil && req.EmailVerified == nil && req.EmailPublic == nil &&
		req.PhoneNumber == nil && req.PhoneNumberPublic == nil &&
		req.DisplayName == nil && req.Bio == nil && req.Gender == nil &&
		req.AvatarAssetID == nil && req.DefaultAvatarKey == nil &&
		req.Language == nil && req.Status == nil {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "at least one user setting is required")
		return
	}

	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Unix()}
	if req.Username != nil {
		username := strings.TrimSpace(*req.Username)
		if !isValidUsername(username) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "username must be 3-32 chars, alphanumeric/underscore/hyphen")
			return
		}
		sets = append(sets, "username = ?", "username_normalized = ?", "username_updated_at = ?")
		args = append(args, username, strings.ToLower(username), time.Now().Unix())
	}
	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if !isValidEmail(email) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid email")
			return
		}
		sets = append(sets, "email = ?", "email_normalized = ?", "email_verified = 0")
		args = append(args, email, strings.ToLower(email))
	}
	if req.EmailVerified != nil {
		sets = append(sets, "email_verified = ?")
		args = append(args, *req.EmailVerified)
	}
	if req.EmailPublic != nil {
		sets = append(sets, "email_public = ?")
		args = append(args, *req.EmailPublic)
	}
	if req.PhoneNumber != nil {
		phone := strings.TrimSpace(*req.PhoneNumber)
		if phone == "" {
			sets = append(sets, "phone_number = NULL", "phone_number_normalized = NULL")
		} else {
			normalized, ok := normalizePhoneNumber(phone)
			if !ok {
				errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid phone number")
				return
			}
			sets = append(sets, "phone_number = ?", "phone_number_normalized = ?")
			args = append(args, phone, normalized)
		}
	}
	if req.PhoneNumberPublic != nil {
		sets = append(sets, "phone_number_public = ?")
		args = append(args, *req.PhoneNumberPublic)
	}
	if req.DisplayName != nil {
		displayName := strings.TrimSpace(*req.DisplayName)
		if displayName == "" || utf8.RuneCountInString(displayName) > 32 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "display_name must be 1-32 characters")
			return
		}
		sets = append(sets, "display_name = ?")
		args = append(args, displayName)
	}
	if req.Bio != nil {
		bio := strings.TrimSpace(*req.Bio)
		if len([]rune(bio)) > 500 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "bio must be at most 500 characters")
			return
		}
		sets = append(sets, "bio = ?")
		args = append(args, bio)
	}
	if req.Gender != nil {
		gender := strings.TrimSpace(*req.Gender)
		if gender != "male" && gender != "female" && gender != "secret" {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "gender must be male, female or secret")
			return
		}
		sets = append(sets, "gender = ?")
		args = append(args, gender)
	}
	if req.AvatarAssetID != nil {
		assetID := strings.TrimSpace(*req.AvatarAssetID)
		var filename sql.NullString
		if assetID != "" {
			_ = h.DB.QueryRow(`SELECT filename FROM assets WHERE id = ?`, assetID).Scan(&filename)
		}
		sets = append(sets, "avatar_url = ?")
		if filename.Valid {
			args = append(args, assetURL(assetID, filename.String))
		} else {
			args = append(args, nil)
		}
	}
	if req.DefaultAvatarKey != nil {
		key := strings.TrimSpace(*req.DefaultAvatarKey)
		if !isValidAvatarKey(key) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid default avatar key")
			return
		}
		sets = append(sets, "default_avatar_key = ?")
		args = append(args, key)
	}
	if req.Language != nil {
		language := strings.TrimSpace(*req.Language)
		if !isValidLanguage(language) {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "language must be zh-Hans, zh-Hant or en")
			return
		}
		sets = append(sets, "language = ?")
		args = append(args, language)
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != "active" && status != "deleted" && status != "suspended" {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid status")
			return
		}
		sets = append(sets, "status = ?")
		args = append(args, status)
		if status == "deleted" {
			sets = append(sets, "deleted_at = ?")
			args = append(args, time.Now().Unix())
		}
		if status == "active" {
			sets = append(sets, "deleted_at = NULL")
		}
	}

	targetID := c.Param("user_id")
	if model.IsSuperuser(h.DB, targetID) && req.Status != nil && strings.TrimSpace(*req.Status) != "active" {
		errorJSON(c, http.StatusForbidden, "forbidden", "super user account cannot be disabled")
		return
	}
	args = append(args, targetID)
	tx, err := h.DB.Begin()
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "force update user failed")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		if isDuplicateEntryError(err) {
			errorJSON(c, http.StatusConflict, "conflict", "username, email or phone number already taken")
			return
		}
		errorJSON(c, http.StatusInternalServerError, "internal_error", "force update user failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		errorJSON(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if req.Status != nil && strings.TrimSpace(*req.Status) != "active" {
		if _, err := tx.Exec(`UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, time.Now().Unix(), targetID); err != nil {
			errorJSON(c, http.StatusInternalServerError, "internal_error", "force update user failed")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "force update user failed")
		return
	}
	user, err := model.GetUserByID(h.DB, targetID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userResponse(user)})
}

func isValidLanguage(value string) bool {
	return value == "zh-Hans" || value == "zh-Hant" || value == "en"
}

func normalizePhoneNumber(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	var normalized strings.Builder
	for i, r := range value {
		if r >= '0' && r <= '9' {
			normalized.WriteRune(r)
			continue
		}
		if r == '+' && i == 0 {
			normalized.WriteRune(r)
			continue
		}
		if r == ' ' || r == '-' || r == '(' || r == ')' {
			continue
		}
		return "", false
	}
	text := normalized.String()
	digitCount := 0
	for _, r := range text {
		if r >= '0' && r <= '9' {
			digitCount++
		}
	}
	return text, digitCount >= 5 && digitCount <= 20
}

func isValidAvatarKey(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func assetURL(assetID, filename string) string {
	return "/assets/" + strings.Trim(strings.TrimSpace(assetID), "/") + "/" + strings.Trim(strings.TrimSpace(filename), "/")
}
