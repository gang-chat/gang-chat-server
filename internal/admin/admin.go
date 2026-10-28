// Package admin serves the admin console's API under /api/v1/admin: accounts,
// a read-only view of the database, the server's storage directory, release
// sync and the client releases the homepage shows. The console signs in with POST /admin/login and
// sends the token it gets back on every other request.
//
// Responses are {"data": ...}; errors use the API's usual {"error": {...}}.
package admin

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

type Handler struct {
	DB      *sql.DB
	Config  *config.Config
	Objects objectStore
	// Bucket names the storage directory in the file browser.
	Bucket string
	Sync   *releaseSyncConfig
	// GitHubAPI is the GitHub REST root; tests point it at a fake.
	GitHubAPI  string
	HTTPClient *http.Client

	// syncing keeps one release sync at a time: the console's and the release
	// watch's would clear each other's files.
	syncing sync.Mutex

	// disabledReason, when set, is why the console can't sign in.
	disabledReason string
	limiter        loginLimiter
}

// RegisterRoutes mounts the admin API on g (/api/v1). It returns an error for
// release sync settings that don't parse; missing or weak console credentials
// only turn the console off, so the chat server still starts.
func RegisterRoutes(g *gin.RouterGroup, db *sql.DB, cfg *config.Config, files *storage.Files) (*Handler, error) {
	releaseSync, err := newReleaseSyncConfig(cfg)
	if err != nil {
		return nil, err
	}
	h := &Handler{
		DB:         db,
		Config:     cfg,
		Sync:       releaseSync,
		GitHubAPI:  "https://api.github.com",
		HTTPClient: &http.Client{},
	}
	if files != nil {
		h.Objects = fileStore{files: files}
		h.Bucket = files.Root()
	}
	switch {
	case strings.TrimSpace(cfg.AdminUsername) == "" || cfg.AdminPassword == "":
		h.disabledReason = "Set admin_username and admin_password in the server's config.json"
	case len(passwordPolicyIssues(cfg.AdminPassword, cfg.AdminUsername)) > 0:
		h.disabledReason = "admin_password in the server's config.json is too weak: " +
			strings.Join(passwordPolicyIssues(cfg.AdminPassword, cfg.AdminUsername), "; ")
	}
	if h.disabledReason != "" {
		log.Printf("admin console off: %s", h.disabledReason)
	}
	h.register(g)
	return h, nil
}

func (h *Handler) register(g *gin.RouterGroup) {
	g.GET("/admin/status", h.handle(h.status))
	g.POST("/admin/login", h.handle(h.login))
	a := g.Group("/admin", h.requireSession)
	a.GET("/me", h.handle(h.me))
	a.POST("/logout", h.handle(h.logout))
	a.GET("/overview", h.handle(h.overview))

	a.GET("/db/tables", h.handle(h.listTables))
	a.GET("/db/tables/:table/schema", h.handle(h.describeTable))
	a.GET("/db/tables/:table/rows", h.handle(h.readRows))

	a.GET("/storage/objects", h.handle(h.listObjects))
	a.GET("/storage/objects/head", h.handle(h.headObject))
	a.GET("/storage/objects/download", h.handle(h.downloadObject))
	a.POST("/storage/objects", h.handle(h.uploadObject))
	a.DELETE("/storage/objects", h.handle(h.deleteObject))

	a.GET("/release-sync/releases", h.handle(h.listGitHubReleases))
	a.POST("/release-sync", h.handle(h.syncRelease))

	a.GET("/releases", h.handle(h.listReleases))
	a.POST("/releases", h.handle(h.createRelease))
	a.PATCH("/releases/:version", h.handle(h.updateRelease))
	a.DELETE("/releases/:version", h.handle(h.deleteRelease))

	a.GET("/accounts", h.handle(h.listAccounts))
	a.GET("/accounts/rooms", h.handle(h.listRooms))
	a.GET("/accounts/:id", h.handle(h.getAccount))
	a.POST("/accounts", h.handle(h.createAccount))
	a.PATCH("/accounts/:id", h.handle(h.updateAccount))
	a.POST("/accounts/:id/password", h.handle(h.resetPassword))
	a.POST("/accounts/:id/sessions/revoke", h.handle(h.revokeSessions))
	a.DELETE("/accounts/:id", h.handle(h.deleteAccount))
	a.POST("/accounts/:id/memberships", h.handle(h.addMembership))
	a.PATCH("/accounts/:id/memberships/:roomId", h.handle(h.updateMembership))
	a.DELETE("/accounts/:id/memberships/:roomId", h.handle(h.removeMembership))
}

// apiError is a failure the console shows as is.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func fail(status int, code, message string) error {
	return &apiError{status: status, code: code, message: message}
}

func writeError(c *gin.Context, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		log.Printf("admin: %s %s: %v", c.Request.Method, c.FullPath(), err)
		apiErr = &apiError{status: http.StatusInternalServerError, code: "INTERNAL_ERROR", message: "Admin request failed"}
	}
	c.AbortWithStatusJSON(apiErr.status, gin.H{"error": gin.H{
		"code":       apiErr.code,
		"message":    apiErr.message,
		"request_id": c.GetString("request_id"),
	}})
}

// handle wraps a handler that returns its data, or writes the response itself
// and returns nil (downloads).
func (h *Handler) handle(fn func(c *gin.Context) (any, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		data, err := fn(c)
		if err != nil {
			writeError(c, err)
			return
		}
		if c.Writer.Written() {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": data})
	}
}

// audit records a change the console made, or tried to make.
func (h *Handler) audit(c *gin.Context, action, target, detail string, err error) {
	h.record(c.GetString(actorKey), action, target, detail, err)
}

// record writes one audit row for actor.
func (h *Handler) record(actor, action, target, detail string, err error) {
	status := "ok"
	if err != nil {
		status = "failed"
		detail = "internal error"
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			detail = apiErr.message
		}
	}
	if utf8.RuneCountInString(detail) > 500 {
		detail = string([]rune(detail)[:500])
	}
	if _, dbErr := h.DB.Exec(
		`INSERT INTO admin_audit_log (created_at, actor, action, target, status, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), actor, action, target, status, detail,
	); dbErr != nil {
		log.Printf("admin: record %s audit: %v", action, dbErr)
	}
}

type overview struct {
	Database struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
	} `json:"database"`
	Storage struct {
		Configured bool   `json:"configured"`
		Endpoint   string `json:"endpoint"`
		// Bucket is the storage directory.
		Bucket string `json:"bucket"`
	} `json:"storage"`
	ReleaseSync releaseSyncSummary `json:"releaseSync"`
}

func (h *Handler) overview(c *gin.Context) (any, error) {
	var out overview
	out.Database.Path = h.Config.DatabasePath
	if info, err := os.Stat(h.Config.DatabasePath); err == nil {
		out.Database.SizeBytes = info.Size()
	}
	out.Storage.Configured = h.Objects != nil
	out.Storage.Bucket = h.Bucket
	out.ReleaseSync = h.Sync.summary()
	return out, nil
}

// bindJSON decodes the request body into dest.
func bindJSON(c *gin.Context, dest any) error {
	if err := c.ShouldBindJSON(dest); err != nil {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Invalid JSON body")
	}
	return nil
}
