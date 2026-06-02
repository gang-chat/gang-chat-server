package chat

import (
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	filename := safeAssetFilename(header.Filename, mimeType)
	assetDir := "assets"
	if h.Cfg != nil && h.Cfg.AssetDir != "" {
		assetDir = h.Cfg.AssetDir
	}
	diskDir := filepath.Join(assetDir, id)
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "create asset directory failed")
		return
	}
	diskPath := filepath.Join(diskDir, filename)
	sizeBytes, detectedMime, err := writeUploadedAsset(diskPath, file)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "store uploaded file failed")
		return
	}
	if detectedMime != "" && detectedMime != "application/octet-stream" {
		mimeType = detectedMime
	}
	if strings.Contains(c.FullPath(), "/uploads/images") && !strings.HasPrefix(mimeType, "image/") {
		_ = os.Remove(diskPath)
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "image file is required")
		return
	}
	now := nowMillis()
	url := "/assets/" + id + "/" + filename
	thumb := url
	_, err = h.DB.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, thumbnail_url, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, currentUserID(c), purpose, filename, mimeType, sizeBytes, url, thumb, now,
	)
	if err != nil {
		_ = os.Remove(diskPath)
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

func writeUploadedAsset(path string, src io.Reader) (int64, string, error) {
	dst, err := os.Create(path)
	if err != nil {
		return 0, "", err
	}
	defer dst.Close()

	var sniff [512]byte
	n, readErr := src.Read(sniff[:])
	if readErr != nil && readErr != io.EOF {
		return 0, "", readErr
	}
	var written int64
	if n > 0 {
		count, err := dst.Write(sniff[:n])
		if err != nil {
			return 0, "", err
		}
		written += int64(count)
	}
	rest, err := io.Copy(dst, src)
	if err != nil {
		return 0, "", err
	}
	written += rest
	mimeType := ""
	if n > 0 {
		mimeType = http.DetectContentType(sniff[:n])
	}
	return written, mimeType, nil
}

func safeAssetFilename(name, mimeType string) string {
	base := filepath.Base(strings.TrimSpace(name))
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" || stem == "." {
		stem = "file"
	}
	var cleaned strings.Builder
	for _, r := range stem {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			cleaned.WriteRune(r)
		} else {
			cleaned.WriteByte('-')
		}
	}
	value := strings.Trim(cleaned.String(), "-_")
	if value == "" {
		value = "file"
	}
	if ext == "" {
		switch mimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/webp":
			ext = ".webp"
		case "image/gif":
			ext = ".gif"
		default:
			ext = ".png"
		}
	}
	return value + ext
}
