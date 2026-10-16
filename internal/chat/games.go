package chat

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Tickets for gang-games-server, the mini-game lobby the client opens in a
// web view. A ticket is a short-lived JWT signed with Ed25519 that says
// "this user is in this room"; the games server checks it with the public
// key and never needs this database. The claims must match what it
// verifies (gang-games-server src/auth.ts).
const (
	gamesTicketIssuer   = "gang-chat"
	gamesTicketAudience = "gang-games"
	gamesTicketTTL      = 60 * time.Second
)

type gamesTicketKey struct {
	once sync.Once
	key  ed25519.PrivateKey
	err  error
}

var errGamesNotConfigured = errors.New("games_ticket_private_key_file is not set")

func (h *Handler) gamesTicketSigningKey() (ed25519.PrivateKey, error) {
	h.gamesKey.once.Do(func() {
		h.gamesKey.key, h.gamesKey.err = loadGamesTicketKey(h.Cfg.GamesTicketPrivateKeyFile)
	})
	return h.gamesKey.key, h.gamesKey.err
}

func loadGamesTicketKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errGamesNotConfigured
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read games ticket key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("games ticket key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse games ticket key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("games ticket key is not Ed25519")
	}
	return key, nil
}

// issueGamesTicket returns a ticket for the caller in this room, for
// gang-games-server's shell page (#ticket=…).
func (h *Handler) issueGamesTicket(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	// Like voice: members, and superusers who can drop into any room.
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	key, err := h.gamesTicketSigningKey()
	if err != nil {
		if !errors.Is(err, errGamesNotConfigured) {
			log.Printf("games ticket: %v", err)
		}
		h.jsonError(c, http.StatusServiceUnavailable, "games_unavailable", "games are not available on this server")
		return
	}
	user, err := h.userSummaryForRoom(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "load user failed")
		return
	}
	now := time.Now()
	expires := now.Add(gamesTicketTTL)
	ticket, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss":  gamesTicketIssuer,
		"aud":  gamesTicketAudience,
		"sub":  userID,
		"name": firstNonEmptyString(dereferenceString(user.RoomDisplayName), user.DisplayName, user.Username),
		"room": roomID,
		"iat":  now.Unix(),
		"exp":  expires.Unix(),
	}).SignedString(key)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "sign ticket failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ticket": ticket, "expires_at": expires.UTC().Format(time.RFC3339)})
}
