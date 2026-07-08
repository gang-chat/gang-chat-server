package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestServerTimeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ServerTime())
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ping", nil)
	router.ServeHTTP(recorder, request)

	raw := recorder.Header().Get(ServerTimeHeader)
	if raw == "" {
		t.Fatalf("expected %s header", ServerTimeHeader)
	}

	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("expected RFC3339Nano server time, got %q: %v", raw, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("expected UTC server time, got %s", parsed.Location())
	}
}

func TestCORSExposesServerTimeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(CORS("*"))
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ping", nil)
	router.ServeHTTP(recorder, request)

	exposed := recorder.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposed, ServerTimeHeader) {
		t.Fatalf("expected exposed headers %q to include %s", exposed, ServerTimeHeader)
	}
}
