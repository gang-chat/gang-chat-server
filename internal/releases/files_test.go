package releases

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

func TestReleaseFilesAreServedNextToTheChatAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	files, err := storage.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for key, body := range map[string]string{
		storage.ReleasePrefix + "latest.yml":          "path: GangChat_v1.0.0.exe\n",
		storage.ReleasePrefix + "GangChat_v1.0.0.exe": "0123456789",
	} {
		if err := files.Put(key, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	// The chat's own assets share the /assets prefix.
	router.GET("/assets/:asset_id/:filename", func(c *gin.Context) { c.String(http.StatusOK, "asset "+c.Param("asset_id")) })
	RegisterFileRoutes(router, files)
	get := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := get(http.MethodGet, "/assets/releases/latest.yml", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "path: GangChat_v1.0.0.exe\n" ||
		rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("Content-Type") != "text/yaml" {
		t.Fatalf("feed: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = get(http.MethodGet, "/assets/releases/GangChat_v1.0.0.exe", map[string]string{"Range": "bytes=4-"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "456789" ||
		rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("a download must resume: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = get(http.MethodHead, "/assets/releases/GangChat_v1.0.0.exe", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "10" || rec.Body.Len() != 0 {
		t.Fatalf("head: %d %v", rec.Code, rec.Header())
	}
	for _, path := range []string{"/assets/releases/GangChat_v0.9.0.exe", "/assets/releases/.put-1"} {
		if rec := get(http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	if rec := get(http.MethodGet, "/assets/ast_1/a.png", nil); rec.Body.String() != "asset ast_1" {
		t.Fatalf("chat assets must keep their route: %q", rec.Body.String())
	}
}

func TestFileContentType(t *testing.T) {
	for name, want := range map[string]string{
		"GangChat_v1.0.0.apk":          "application/vnd.android.package-archive",
		"GangChat_v1.0.0.DMG":          "application/x-apple-diskimage",
		"GangChat_v1.0.0.exe.blockmap": "application/octet-stream",
		"appcast.xml":                  "application/xml",
		"notes":                        "application/octet-stream",
	} {
		if got := FileContentType(name); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
}
