package auth

import (
	"net/http/httptest"
	"testing"
)

func TestRequestClientIPUsesForwardedForFromTrustedLoopbackProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:52110"
	req.Header.Set("X-Forwarded-For", "203.0.113.24, 127.0.0.1")

	got := requestClientIP(req, []string{"127.0.0.1"})
	if got != "203.0.113.24" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "203.0.113.24")
	}
}

func TestRequestClientIPUsesRealIPFromTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[::1]:52110"
	req.Header.Set("X-Real-IP", "198.51.100.9")

	got := requestClientIP(req, []string{"::1"})
	if got != "198.51.100.9" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "198.51.100.9")
	}
}

func TestRequestClientIPUsesCloudflareHeaderFromTrustedCIDR(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.4.5.6:52110"
	req.Header.Set("CF-Connecting-IP", "198.51.100.12")

	got := requestClientIP(req, []string{"10.0.0.0/8"})
	if got != "198.51.100.12" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "198.51.100.12")
	}
}

func TestRequestClientIPParsesForwardedHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:52110"
	req.Header.Set("Forwarded", `for="[2001:db8::7]:443";proto=https`)

	got := requestClientIP(req, []string{"127.0.0.1"})
	if got != "2001:db8::7" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "2001:db8::7")
	}
}

func TestRequestClientIPIgnoresHeadersFromUntrustedRemote(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.50:52110"
	req.Header.Set("X-Forwarded-For", "203.0.113.24")

	got := requestClientIP(req, []string{"127.0.0.1"})
	if got != "198.51.100.50" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "198.51.100.50")
	}
}

func TestRequestClientIPFallsBackToRemoteWhenTrustedHeadersAreInvalid(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:52110"
	req.Header.Set("X-Forwarded-For", "unknown, not-an-ip")

	got := requestClientIP(req, []string{"127.0.0.1"})
	if got != "127.0.0.1" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "127.0.0.1")
	}
}
