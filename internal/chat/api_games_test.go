package chat

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// writeGamesTicketKey makes a key pair, points the config at the private
// half, and returns the public half for verifying tickets.
func writeGamesTicketKey(t *testing.T, h *apiHarness) ed25519.PublicKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ticket-private.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	h.cfg.GamesTicketPrivateKeyFile = path
	if out := os.Getenv("GAMES_TICKET_PUBLIC_KEY_OUT"); out != "" {
		// Lets a cross-check verify these tickets with gang-games-server.
		pub, _ := x509.MarshalPKIXPublicKey(public)
		_ = os.WriteFile(out, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0o600)
	}
	return public
}

func TestGamesTicketForRoomMembers(t *testing.T) {
	h := newAPIHarness(t)
	public := writeGamesTicketKey(t, h)
	alice := h.register("alice")
	bob := h.register("bob")
	room := h.createRoom(alice.Token, map[string]any{"name": "开黑", "visibility": "public", "join_policy": "open"})
	roomID := room["id"].(string)

	status, body := h.request(http.MethodPost, "/rooms/"+roomID+"/games/ticket", alice.Token, map[string]any{})
	h.requireStatus(status, http.StatusOK, body)
	ticket, _ := body["ticket"].(string)
	if out := os.Getenv("GAMES_TICKET_OUT"); out != "" {
		_ = os.WriteFile(out, []byte(ticket), 0o600)
	}

	parsed, err := jwt.Parse(ticket, func(token *jwt.Token) (any, error) { return public, nil },
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("gang-chat"), jwt.WithAudience("gang-games"))
	if err != nil || !parsed.Valid {
		t.Fatalf("ticket does not verify: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["sub"] != alice.User["id"] || claims["room"] != roomID || claims["name"] == "" {
		t.Fatalf("unexpected claims: %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != 60 {
		t.Fatalf("ticket should last 60 seconds, lasts %v", exp-iat)
	}

	// Not a member of the room: no ticket.
	status, body = h.request(http.MethodPost, "/rooms/"+roomID+"/games/ticket", bob.Token, map[string]any{})
	h.requireStatus(status, http.StatusNotFound, body)
}

func TestGamesTicketNeedsAKey(t *testing.T) {
	h := newAPIHarness(t)
	alice := h.register("alice")
	room := h.createRoom(alice.Token, map[string]any{"name": "开黑", "visibility": "public", "join_policy": "open"})
	status, body := h.request(http.MethodPost, "/rooms/"+room["id"].(string)+"/games/ticket", alice.Token, map[string]any{})
	h.requireStatus(status, http.StatusServiceUnavailable, body)
}
