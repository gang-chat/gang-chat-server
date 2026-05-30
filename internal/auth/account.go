package auth

import (
	"database/sql"
	"net/http"
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
	if req.Username == nil && req.Email == nil {
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
		if !strings.Contains(email, "@") || !strings.Contains(email, ".") || len(email) > 254 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid email")
			return
		}
		sets = append(sets, "email = ?", "email_normalized = ?", "email_verified = 0")
		args = append(args, email, strings.ToLower(email))
	}

	args = append(args, user.ID)
	_, err := h.DB.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			errorJSON(c, http.StatusConflict, "conflict", "username or email already taken")
			return
		}
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
	if req.DisplayName == nil && req.Bio == nil && req.AvatarAssetID == nil {
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
	if req.AvatarAssetID != nil {
		var url sql.NullString
		if *req.AvatarAssetID != "" {
			_ = h.DB.QueryRow(`SELECT url FROM assets WHERE id = ? AND owner_user_id = ?`, *req.AvatarAssetID, user.ID).Scan(&url)
		}
		sets = append(sets, "avatar_url = ?")
		if url.Valid {
			args = append(args, url.String)
		} else {
			args = append(args, nil)
		}
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
	rows, err := h.DB.Query(
		`SELECT id, uid, username, display_name, avatar_url, default_avatar_key
		 FROM users
		 WHERE status = 'active'
		   AND (uid = ? OR username_normalized = ? OR instr(lower(COALESCE(display_name, username)), lower(?)) > 0)
		 ORDER BY username ASC
		 LIMIT ?`,
		q, strings.ToLower(q), q, limit,
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
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar); err != nil {
			errorJSON(c, http.StatusInternalServerError, "internal_error", "read user failed")
			return
		}
		users = append(users, gin.H{
			"id": id, "uid": uid, "username": username,
			"display_name":       nullableOr(displayName, username),
			"avatar_url":         nullablePtrString(avatarURL),
			"default_avatar_key": nullableOr(defaultAvatar, "blue-3"),
		})
	}
	c.JSON(http.StatusOK, gin.H{"users": users, "next_cursor": nil})
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
	now := time.Now().Unix()
	if _, err := h.DB.Exec(`UPDATE users SET status = 'deleted', deleted_at = ?, updated_at = ? WHERE id = ?`, now, now, user.ID); err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete account failed")
		return
	}
	_ = model.RevokeAllOtherSessions(h.DB, user.ID, "")
	_ = model.RevokeSession(h.DB, getSessionID(c))
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

type forceUserSettingsRequest struct {
	Username      *string `json:"username"`
	Email         *string `json:"email"`
	DisplayName   *string `json:"display_name"`
	Bio           *string `json:"bio"`
	AvatarAssetID *string `json:"avatar_asset_id"`
	Status        *string `json:"status"`
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
	if req.Username == nil && req.Email == nil && req.DisplayName == nil && req.Bio == nil && req.AvatarAssetID == nil && req.Status == nil {
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
		if !strings.Contains(email, "@") || !strings.Contains(email, ".") || len(email) > 254 {
			errorJSON(c, http.StatusBadRequest, "validation_failed", "invalid email")
			return
		}
		sets = append(sets, "email = ?", "email_normalized = ?", "email_verified = 0")
		args = append(args, email, strings.ToLower(email))
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
	if req.AvatarAssetID != nil {
		var url sql.NullString
		if *req.AvatarAssetID != "" {
			_ = h.DB.QueryRow(`SELECT url FROM assets WHERE id = ?`, *req.AvatarAssetID).Scan(&url)
		}
		sets = append(sets, "avatar_url = ?")
		if url.Valid {
			args = append(args, url.String)
		} else {
			args = append(args, nil)
		}
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
	res, err := h.DB.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			errorJSON(c, http.StatusConflict, "conflict", "username or email already taken")
			return
		}
		errorJSON(c, http.StatusInternalServerError, "internal_error", "force update user failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		errorJSON(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	user, err := model.GetUserByID(h.DB, targetID)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "failed to read user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userResponse(user)})
}
