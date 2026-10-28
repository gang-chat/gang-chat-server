package releases

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

// The update feeds release sync stores next to the installers, under fixed
// names that installed clients poll.
const (
	SparkleAppcastName      = "appcast.xml" // macOS, Sparkle
	ElectronUpdaterFeedName = "latest.yml"  // Windows, electron-updater
)

// IsUpdateFeed reports whether name is one of the update feeds.
func IsUpdateFeed(name string) bool {
	return name == SparkleAppcastName || name == ElectronUpdaterFeedName
}

var fileContentTypes = map[string]string{
	".apk":      "application/vnd.android.package-archive",
	".dmg":      "application/x-apple-diskimage",
	".exe":      "application/vnd.microsoft.portable-executable",
	".blockmap": "application/octet-stream",
	".xml":      "application/xml",
	".yml":      "text/yaml",
}

// FileContentType is the type a release file is served with.
func FileContentType(name string) string {
	if contentType, ok := fileContentTypes[strings.ToLower(path.Ext(name))]; ok {
		return contentType
	}
	return "application/octet-stream"
}

// RegisterFileRoutes serves the release files in files' storage.ReleasePrefix
// at /assets/releases/<name>: the homepage's downloads, and the feeds and
// installers the clients update from. Range requests resume a download.
func RegisterFileRoutes(r gin.IRouter, files *storage.Files) {
	handler := func(c *gin.Context) {
		name := c.Param("filename")
		file, info, err := files.Open(storage.ReleasePrefix + name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, storage.ErrInvalidKey) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "release file not found"})
				return
			}
			log.Printf("releases: open %s: %v", name, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "read release file failed"})
			return
		}
		defer file.Close()
		// Installed apps must see a new feed as soon as it lands; installers
		// and blockmaps are named after their version.
		if IsUpdateFeed(name) {
			c.Header("Cache-Control", "no-cache")
		} else {
			c.Header("Cache-Control", "public, max-age=86400")
		}
		c.Header("Content-Type", FileContentType(name))
		http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
	}
	r.GET("/assets/releases/:filename", handler)
	r.HEAD("/assets/releases/:filename", handler)
}
