package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	gangdb "github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

// Account administration writes the same rows the server writes for the same
// action (registration, the super user's forced settings and password reset,
// account deletion, room membership changes). It doesn't publish realtime
// events or room system messages, so open clients see a change on their next
// request. Session revocation and account status are checked on every request,
// so suspensions and sign-outs take effect at once.

var (
	usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)
	emailPattern    = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

type account struct {
	ID                 string `json:"id"`
	UID                string `json:"uid"`
	Username           string `json:"username"`
	Email              string `json:"email"`
	DisplayName        string `json:"displayName,omitempty"`
	Status             string `json:"status"`
	IsSuperuser        bool   `json:"isSuperuser"`
	EmailVerified      bool   `json:"emailVerified"`
	RoomCount          int    `json:"roomCount"`
	ActiveSessionCount int    `json:"activeSessionCount"`
	LastSeenAt         string `json:"lastSeenAt,omitempty"`
	CreatedAt          string `json:"createdAt"`
	UpdatedAt          string `json:"updatedAt"`
}

type membership struct {
	RoomID   string `json:"roomId"`
	RID      string `json:"rid"`
	RoomName string `json:"roomName"`
	Role     string `json:"role"`
	JoinedAt string `json:"joinedAt"`
}

type accountDetail struct {
	account
	Memberships []membership `json:"memberships"`
}

const accountColumns = `u.id, u.uid, u.username, u.email, u.display_name, u.status, u.is_superuser,
	u.email_verified, u.created_at, u.updated_at,
	(SELECT COUNT(*) FROM room_memberships rm WHERE rm.user_id = u.id) AS room_count,
	(SELECT COUNT(*) FROM user_sessions s
	  WHERE s.user_id = u.id AND s.revoked_at IS NULL AND s.expires_at > ?) AS active_sessions,
	(SELECT MAX(s.last_used_at) FROM user_sessions s WHERE s.user_id = u.id) AS last_seen_at`

func scanAccount(row interface{ Scan(...any) error }) (account, error) {
	var a account
	var displayName sql.NullString
	var superuser, verified int
	var created, updated int64
	var lastSeen sql.NullInt64
	err := row.Scan(&a.ID, &a.UID, &a.Username, &a.Email, &displayName, &a.Status, &superuser,
		&verified, &created, &updated, &a.RoomCount, &a.ActiveSessionCount, &lastSeen)
	a.DisplayName = displayName.String
	a.IsSuperuser = superuser != 0
	a.EmailVerified = verified != 0
	a.CreatedAt = toISO(created)
	a.UpdatedAt = toISO(updated)
	if lastSeen.Valid {
		a.LastSeenAt = toISO(lastSeen.Int64)
	}
	return a, err
}

// toISO formats a stored time; users and sessions store unix seconds, rooms
// and memberships milliseconds.
func toISO(value int64) string {
	if value <= 0 {
		return ""
	}
	if value < 100_000_000_000 {
		value *= 1000
	}
	return time.UnixMilli(value).UTC().Format("2006-01-02T15:04:05.000Z")
}

// escapeLike escapes LIKE wildcards for patterns that use ESCAPE '\'.
func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func (h *Handler) listAccounts(c *gin.Context) (any, error) {
	var where []string
	var args []any
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		if utf8.RuneCountInString(q) > 320 {
			return nil, fail(http.StatusBadRequest, "VALIDATION_ERROR", "Search is too long")
		}
		like := "%" + escapeLike(model.Normalize(q)) + "%"
		where = append(where, `(u.username_normalized LIKE ? ESCAPE '\' OR u.email_normalized LIKE ? ESCAPE '\'
			OR u.display_name LIKE ? ESCAPE '\' OR u.uid = ? OR u.id = ?)`)
		args = append(args, like, like, "%"+escapeLike(q)+"%", q, q)
	}
	switch status := c.DefaultQuery("status", "all"); status {
	case "all":
	case "active", "suspended", "deleted":
		where = append(where, "u.status = ?")
		args = append(args, status)
	default:
		return nil, fail(http.StatusBadRequest, "VALIDATION_ERROR", "Unknown status")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM users u `+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	limit := clampQueryInt(c, "limit", 50, 1, 200)
	offset := clampQueryInt(c, "offset", 0, 0, 1<<31-1)
	rows, err := h.DB.Query(
		`SELECT `+accountColumns+` FROM users u `+whereSQL+`
		 ORDER BY u.created_at DESC, u.id LIMIT ? OFFSET ?`,
		append(append([]any{time.Now().Unix()}, args...), limit, offset)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return gin.H{"total": total, "items": items}, nil
}

func (h *Handler) loadAccount(id string) (*accountDetail, error) {
	a, err := scanAccount(h.DB.QueryRow(`SELECT `+accountColumns+` FROM users u WHERE u.id = ?`, time.Now().Unix(), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fail(http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found")
	}
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Query(
		`SELECT rm.room_id, r.rid, r.name, rm.role, rm.joined_at
		 FROM room_memberships rm JOIN rooms r ON r.id = rm.room_id
		 WHERE rm.user_id = ?
		 ORDER BY CASE rm.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, rm.joined_at`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	detail := &accountDetail{account: a, Memberships: []membership{}}
	for rows.Next() {
		var m membership
		var joined int64
		if err := rows.Scan(&m.RoomID, &m.RID, &m.RoomName, &m.Role, &joined); err != nil {
			return nil, err
		}
		m.JoinedAt = toISO(joined)
		detail.Memberships = append(detail.Memberships, m)
	}
	return detail, rows.Err()
}

func (h *Handler) getAccount(c *gin.Context) (any, error) {
	return h.loadAccount(c.Param("id"))
}

func (h *Handler) listRooms(c *gin.Context) (any, error) {
	term := strings.TrimSpace(c.Query("q"))
	query := `SELECT r.id, r.rid, r.name,
	        (SELECT COUNT(*) FROM room_memberships rm WHERE rm.room_id = r.id) AS member_count
	 FROM rooms r`
	var args []any
	if term != "" {
		query += ` WHERE r.name LIKE ? ESCAPE '\' OR r.rid = ? OR r.id = ?`
		args = append(args, "%"+escapeLike(term)+"%", term, term)
	}
	rows, err := h.DB.Query(query+` ORDER BY r.created_at DESC LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type roomSummary struct {
		ID          string `json:"id"`
		RID         string `json:"rid"`
		Name        string `json:"name"`
		MemberCount int    `json:"memberCount"`
	}
	rooms := []roomSummary{}
	for rows.Next() {
		var r roomSummary
		if err := rows.Scan(&r.ID, &r.RID, &r.Name, &r.MemberCount); err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, rows.Err()
}

func checkUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Username must be 3-32 letters, digits, underscores or hyphens")
	}
	return nil
}

func checkEmail(email string) error {
	if len(email) > 254 || !emailPattern.MatchString(email) {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Enter a valid email address")
	}
	return nil
}

func checkDisplayName(name string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > 32 {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Display name must be 1-32 characters")
	}
	return nil
}

func checkPassword(password string) error {
	if len(password) < 8 || len(password) > 256 {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Password must be 8-256 characters")
	}
	return nil
}

func conflictIfDuplicate(err error) error {
	if gangdb.IsUniqueViolation(err) {
		return fail(http.StatusConflict, "ACCOUNT_CONFLICT", "Username or email is already taken")
	}
	return err
}

type lockedUser struct {
	ID          string
	UID         string
	Username    string
	IsSuperuser bool
}

// lockUser reads a user inside tx. Transactions begin IMMEDIATE, so the row
// can't change before the transaction ends.
func lockUser(tx *sql.Tx, id string) (lockedUser, error) {
	var u lockedUser
	var superuser int
	err := tx.QueryRow(`SELECT id, uid, username, is_superuser FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.UID, &u.Username, &superuser)
	if errors.Is(err, sql.ErrNoRows) {
		return u, fail(http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found")
	}
	u.IsSuperuser = superuser != 0
	return u, err
}

func refuseSuperuser(u lockedUser, message string) error {
	if u.IsSuperuser {
		return fail(http.StatusForbidden, "SUPERUSER_READ_ONLY", message)
	}
	return nil
}

func revokeUserSessions(tx *sql.Tx, userID string) (int64, error) {
	result, err := tx.Exec(`UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, time.Now().Unix(), userID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// write runs fn in a transaction and audits it.
func (h *Handler) write(c *gin.Context, action, target string, fn func(tx *sql.Tx) (any, string, error)) (any, error) {
	result, detail, err := func() (any, string, error) {
		tx, err := h.DB.Begin()
		if err != nil {
			return nil, "", err
		}
		defer tx.Rollback()
		result, detail, err := fn(tx)
		if err != nil {
			return nil, "", err
		}
		return result, detail, tx.Commit()
	}()
	h.audit(c, action, target, detail, err)
	return result, err
}

type accountCreateInput struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

// createAccount does what registration does, minus the email code.
func (h *Handler) createAccount(c *gin.Context) (any, error) {
	var in accountCreateInput
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(in.Username)
	email := strings.TrimSpace(in.Email)
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = username
	}
	id := uuid.NewString()
	_, err := func() (any, error) {
		for _, check := range []error{checkUsername(username), checkEmail(email), checkDisplayName(displayName), checkPassword(in.Password)} {
			if check != nil {
				h.audit(c, "accounts.create", username, "", check)
				return nil, check
			}
		}
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			return nil, err
		}
		// NextUserUID takes the write lock itself, so it runs before ours.
		uid, err := idgen.NextUserUID(h.DB)
		if err != nil {
			return nil, err
		}
		return h.write(c, "accounts.create", username, func(tx *sql.Tx) (any, string, error) {
			if err := model.InsertUser(tx, id, uid, username, model.Normalize(username), email, model.Normalize(email), hash); err != nil {
				return nil, "", conflictIfDuplicate(err)
			}
			if displayName != username {
				if _, err := tx.Exec(`UPDATE users SET display_name = ? WHERE id = ?`, displayName, id); err != nil {
					return nil, "", err
				}
			}
			return nil, "uid " + uid, nil
		})
	}()
	if err != nil {
		return nil, err
	}
	return h.loadAccount(id)
}

type accountUpdateInput struct {
	Username      *string `json:"username"`
	Email         *string `json:"email"`
	DisplayName   *string `json:"displayName"`
	EmailVerified *bool   `json:"emailVerified"`
	Status        *string `json:"status"`
}

// updateAccount does what the super user's forced settings update does.
func (h *Handler) updateAccount(c *gin.Context) (any, error) {
	id := c.Param("id")
	var in accountUpdateInput
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var sets, changed []string
	var args []any
	verified := in.EmailVerified
	check := func() error {
		if in.Username != nil {
			username := strings.TrimSpace(*in.Username)
			if err := checkUsername(username); err != nil {
				return err
			}
			sets = append(sets, "username = ?", "username_normalized = ?", "username_updated_at = ?")
			args = append(args, username, model.Normalize(username), now)
			changed = append(changed, "username")
		}
		if in.Email != nil {
			email := strings.TrimSpace(*in.Email)
			if err := checkEmail(email); err != nil {
				return err
			}
			sets = append(sets, "email = ?", "email_normalized = ?")
			args = append(args, email, model.Normalize(email))
			changed = append(changed, "email")
			if verified == nil {
				unverified := false
				verified = &unverified
			}
		}
		if verified != nil {
			value := 0
			if *verified {
				value = 1
			}
			sets = append(sets, "email_verified = ?")
			args = append(args, value)
			if in.EmailVerified != nil {
				changed = append(changed, "emailVerified")
			}
		}
		if in.DisplayName != nil {
			name := strings.TrimSpace(*in.DisplayName)
			if err := checkDisplayName(name); err != nil {
				return err
			}
			sets = append(sets, "display_name = ?")
			args = append(args, name)
			changed = append(changed, "displayName")
		}
		if in.Status != nil {
			switch *in.Status {
			case "active":
				sets = append(sets, "status = 'active'", "deleted_at = NULL")
			case "suspended":
				sets = append(sets, "status = 'suspended'")
			default:
				return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Status must be active or suspended")
			}
			changed = append(changed, "status")
		}
		if len(sets) == 0 {
			return fail(http.StatusBadRequest, "EMPTY_PATCH", "Change at least one account field")
		}
		return nil
	}
	if err := check(); err != nil {
		h.audit(c, "accounts.update", id, "", err)
		return nil, err
	}
	_, err := h.write(c, "accounts.update", id, func(tx *sql.Tx) (any, string, error) {
		user, err := lockUser(tx, id)
		if err != nil {
			return nil, "", err
		}
		if err := refuseSuperuser(user, "The super user account is managed in the app"); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+`, updated_at = ? WHERE id = ?`, append(args, now, id)...); err != nil {
			return nil, "", conflictIfDuplicate(err)
		}
		if in.Status != nil && *in.Status != "active" {
			if _, err := revokeUserSessions(tx, id); err != nil {
				return nil, "", err
			}
		}
		return nil, strings.Join(changed, ", "), nil
	})
	if err != nil {
		return nil, err
	}
	return h.loadAccount(id)
}

type revokeResult struct {
	Username        string `json:"username"`
	RevokedSessions int64  `json:"revokedSessions"`
}

// resetPassword sets a new password and signs out every session.
func (h *Handler) resetPassword(c *gin.Context) (any, error) {
	id := c.Param("id")
	var in struct {
		Password string `json:"password"`
	}
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	if err := checkPassword(in.Password); err != nil {
		h.audit(c, "accounts.password.reset", id, "", err)
		return nil, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	return h.write(c, "accounts.password.reset", id, func(tx *sql.Tx) (any, string, error) {
		user, err := lockUser(tx, id)
		if err != nil {
			return nil, "", err
		}
		if err := refuseSuperuser(user, "The super user password is changed in the app"); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, time.Now().Unix(), id); err != nil {
			return nil, "", err
		}
		revoked, err := revokeUserSessions(tx, id)
		if err != nil {
			return nil, "", err
		}
		return revokeResult{Username: user.Username, RevokedSessions: revoked},
			fmt.Sprintf("%s; %d sessions signed out", user.Username, revoked), nil
	})
}

func (h *Handler) revokeSessions(c *gin.Context) (any, error) {
	id := c.Param("id")
	return h.write(c, "accounts.sessions.revoke", id, func(tx *sql.Tx) (any, string, error) {
		user, err := lockUser(tx, id)
		if err != nil {
			return nil, "", err
		}
		revoked, err := revokeUserSessions(tx, id)
		if err != nil {
			return nil, "", err
		}
		return revokeResult{Username: user.Username, RevokedSessions: revoked},
			fmt.Sprintf("%s; %d sessions signed out", user.Username, revoked), nil
	})
}

// deleteAccount does what account deletion does: messages keep a snapshot of
// their sender and assets lose their owner before the user row goes.
func (h *Handler) deleteAccount(c *gin.Context) (any, error) {
	id := c.Param("id")
	return h.write(c, "accounts.delete", id, func(tx *sql.Tx) (any, string, error) {
		user, err := lockUser(tx, id)
		if err != nil {
			return nil, "", err
		}
		if err := refuseSuperuser(user, "The super user account cannot be deleted"); err != nil {
			return nil, "", err
		}
		if c.GetHeader("x-ops-confirmation") != user.Username {
			return nil, "", fail(http.StatusBadRequest, "DESTRUCTIVE_CONFIRMATION_REQUIRED", "Type the exact username to delete this account")
		}
		if err := model.BackfillMessageSenderSnapshots(tx, id); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(`UPDATE assets SET owner_user_id = NULL WHERE owner_user_id = ?`, id); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
			return nil, "", err
		}
		return gin.H{"username": user.Username, "uid": user.UID},
			fmt.Sprintf("%s (uid %s)", user.Username, user.UID), nil
	})
}

type lockedRoom struct {
	ID   string
	Name string
}

func lockRoom(tx *sql.Tx, id string) (lockedRoom, error) {
	var r lockedRoom
	err := tx.QueryRow(`SELECT id, name FROM rooms WHERE id = ?`, id).Scan(&r.ID, &r.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fail(http.StatusNotFound, "ROOM_NOT_FOUND", "Room not found")
	}
	return r, err
}

func membershipRole(tx *sql.Tx, roomID, userID string) (string, error) {
	var role string
	err := tx.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

type membershipResult struct {
	Username string `json:"username"`
	Room     string `json:"room"`
	Role     string `json:"role,omitempty"`
}

// addMembership does what adding a room member does: the first member of an
// ownerless room becomes its owner.
func (h *Handler) addMembership(c *gin.Context) (any, error) {
	userID := c.Param("id")
	var in struct {
		RoomID string `json:"roomId"`
		Role   string `json:"role"`
	}
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	roomID := strings.TrimSpace(in.RoomID)
	role := in.Role
	if role == "" {
		role = "member"
	}
	return h.write(c, "accounts.membership.add", userID+" -> "+roomID, func(tx *sql.Tx) (any, string, error) {
		if role != "member" && role != "admin" {
			return nil, "", fail(http.StatusBadRequest, "VALIDATION_ERROR", "Role must be member or admin")
		}
		user, err := lockUser(tx, userID)
		if err != nil {
			return nil, "", err
		}
		if err := refuseSuperuser(user, "The super user can already open every room"); err != nil {
			return nil, "", err
		}
		room, err := lockRoom(tx, roomID)
		if err != nil {
			return nil, "", err
		}
		var blocked int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM room_blacklist WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&blocked); err != nil {
			return nil, "", err
		}
		if blocked > 0 {
			return nil, "", fail(http.StatusConflict, "ROOM_BLACKLISTED", user.Username+" is on this room's blacklist; unblock them in the app first")
		}
		if current, err := membershipRole(tx, roomID, userID); err != nil {
			return nil, "", err
		} else if current != "" {
			return nil, "", fail(http.StatusConflict, "ALREADY_MEMBER", user.Username+" is already in this room")
		}
		var owners int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND role = 'owner'`, roomID).Scan(&owners); err != nil {
			return nil, "", err
		}
		if owners == 0 {
			role = "owner"
		}
		joinedAt := time.Now().UnixMilli()
		if _, err := tx.Exec(`INSERT INTO room_memberships (room_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`, roomID, userID, role, joinedAt); err != nil {
			return nil, "", err
		}
		if role == "owner" {
			if _, err := tx.Exec(`UPDATE rooms SET created_by_user_id = ?, updated_at = ? WHERE id = ?`, userID, joinedAt, roomID); err != nil {
				return nil, "", err
			}
		}
		return membershipResult{Username: user.Username, Room: room.Name, Role: role},
			fmt.Sprintf("%s joined %s as %s", user.Username, room.Name, role), nil
	})
}

// updateMembership does what changing a member's role and transferring a
// room do. The owner keeps the role until someone else is made owner, which
// demotes them to admin.
func (h *Handler) updateMembership(c *gin.Context) (any, error) {
	userID, roomID := c.Param("id"), c.Param("roomId")
	var in struct {
		Role string `json:"role"`
	}
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	return h.write(c, "accounts.membership.role", userID+" -> "+roomID, func(tx *sql.Tx) (any, string, error) {
		if in.Role != "member" && in.Role != "admin" && in.Role != "owner" {
			return nil, "", fail(http.StatusBadRequest, "VALIDATION_ERROR", "Role must be member, admin or owner")
		}
		user, err := lockUser(tx, userID)
		if err != nil {
			return nil, "", err
		}
		room, err := lockRoom(tx, roomID)
		if err != nil {
			return nil, "", err
		}
		current, err := membershipRole(tx, roomID, userID)
		if err != nil {
			return nil, "", err
		}
		if current == "" {
			return nil, "", fail(http.StatusNotFound, "NOT_A_MEMBER", user.Username+" is not in this room")
		}
		result := membershipResult{Username: user.Username, Room: room.Name, Role: in.Role}
		detail := fmt.Sprintf("%s is %s of %s", user.Username, in.Role, room.Name)
		if current == in.Role {
			return result, detail, nil
		}
		if in.Role == "owner" {
			if _, err := tx.Exec(`UPDATE room_memberships SET role = 'admin' WHERE room_id = ? AND role = 'owner' AND user_id <> ?`, roomID, userID); err != nil {
				return nil, "", err
			}
			if _, err := tx.Exec(`UPDATE room_memberships SET role = 'owner' WHERE room_id = ? AND user_id = ?`, roomID, userID); err != nil {
				return nil, "", err
			}
			if _, err := tx.Exec(`UPDATE rooms SET created_by_user_id = ?, updated_at = ? WHERE id = ?`, userID, time.Now().UnixMilli(), roomID); err != nil {
				return nil, "", err
			}
			return result, detail, nil
		}
		if current == "owner" {
			return nil, "", errOwnerLocked
		}
		if _, err := tx.Exec(`UPDATE room_memberships SET role = ? WHERE room_id = ? AND user_id = ?`, in.Role, roomID, userID); err != nil {
			return nil, "", err
		}
		return result, detail, nil
	})
}

var errOwnerLocked = fail(http.StatusConflict, "OWNER_ROLE_LOCKED", "Make another member the owner first; the current owner then becomes an admin")

// removeMembership does what removing a room member does.
func (h *Handler) removeMembership(c *gin.Context) (any, error) {
	userID, roomID := c.Param("id"), c.Param("roomId")
	return h.write(c, "accounts.membership.remove", userID+" -> "+roomID, func(tx *sql.Tx) (any, string, error) {
		user, err := lockUser(tx, userID)
		if err != nil {
			return nil, "", err
		}
		room, err := lockRoom(tx, roomID)
		if err != nil {
			return nil, "", err
		}
		current, err := membershipRole(tx, roomID, userID)
		if err != nil {
			return nil, "", err
		}
		if current == "" {
			return nil, "", fail(http.StatusNotFound, "NOT_A_MEMBER", user.Username+" is not in this room")
		}
		if current == "owner" {
			return nil, "", errOwnerLocked
		}
		if _, err := tx.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, userID); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(`DELETE FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID); err != nil {
			return nil, "", err
		}
		return membershipResult{Username: user.Username, Room: room.Name},
			fmt.Sprintf("%s left %s", user.Username, room.Name), nil
	})
}
