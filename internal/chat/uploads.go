package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) uploadFile(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "file is required")
		return
	}
	defer file.Close()
	purpose := c.PostForm("purpose")
	if purpose == "" {
		if strings.Contains(c.FullPath(), "/uploads/images") {
			purpose = "image"
		} else {
			purpose = "message_file"
		}
	}
	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	id := newID("ast")
	now := nowMillis()
	url := "/assets/" + id + "/" + header.Filename
	thumb := url
	_, err = h.DB.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, thumbnail_url, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, currentUserID(c), purpose, header.Filename, mimeType, header.Size, url, thumb, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "store asset failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"asset": h.assetPayload(id)})
}

func (h *Handler) assetPayload(id string) gin.H {
	var url, thumb, mimeType string
	var width, height sql.NullInt64
	var createdAt int64
	_ = h.DB.QueryRow(`SELECT url, thumbnail_url, mime_type, width, height, created_at FROM assets WHERE id = ?`, id).Scan(&url, &thumb, &mimeType, &width, &height, &createdAt)
	return gin.H{"id": id, "url": url, "thumbnail_url": thumb, "mime_type": mimeType, "width": nullableInt64(width), "height": nullableInt64(height), "created_at": formatMillis(createdAt)}
}
