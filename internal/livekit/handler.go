package livekit

import (
	"net/http"

	"github.com/gin-gonic/gin"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/protocol/livekit"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

type Handler struct {
	RoomClient *lksdk.RoomServiceClient
	Cfg        *config.Config
}

func RegisterRoutes(g *gin.RouterGroup, roomClient *lksdk.RoomServiceClient, cfg *config.Config) {
	h := &Handler{RoomClient: roomClient, Cfg: cfg}

	g.POST("/token", h.createToken)
	g.GET("/rooms", h.listRooms)
	g.POST("/rooms", h.createRoom)
	g.DELETE("/rooms/:name", h.deleteRoom)
	g.GET("/rooms/:name/participants", h.listParticipants)
	g.DELETE("/rooms/:name/participants/:identity", h.removeParticipant)
}

type CreateTokenRequest struct {
	Room string `json:"room" binding:"required"`
}

type LiveKitTokenResponse struct {
	Token     string `json:"token"`
	ServerURL string `json:"server_url"`
}

type CreateRoomRequest struct {
	Name     string `json:"name" binding:"required"`
	Metadata string `json:"metadata,omitempty"`
}

type RoomInfo struct {
	SID             string `json:"sid"`
	Name            string `json:"name"`
	Metadata        string `json:"metadata"`
	NumParticipants int    `json:"num_participants"`
}

type ListRoomsResponse struct {
	Rooms []RoomInfo `json:"rooms"`
}

type ParticipantInfo struct {
	SID      string `json:"sid"`
	Identity string `json:"identity"`
	Name     string `json:"name"`
	State    string `json:"state"`
}

type ListParticipantsResponse struct {
	Participants []ParticipantInfo `json:"participants"`
}

func (h *Handler) createToken(c *gin.Context) {
	var req CreateTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "bad_request", "message": "room is required"}})
		return
	}

	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")

	token, err := GenerateJoinToken(TokenParams{
		APIKey:       h.Cfg.LiveKitAPIKey,
		APISecret:    h.Cfg.LiveKitAPISecret,
		Room:         req.Room,
		Identity:     userID.(string),
		Name:         username.(string),
		CanPublish:   true,
		CanSubscribe: true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "failed to generate token"}})
		return
	}

	c.JSON(http.StatusOK, LiveKitTokenResponse{
		Token:     token,
		ServerURL: h.Cfg.LiveKitHost,
	})
}

func (h *Handler) listRooms(c *gin.Context) {
	res, err := h.RoomClient.ListRooms(c.Request.Context(), &livekit.ListRoomsRequest{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
		return
	}

	rooms := make([]RoomInfo, 0, len(res.Rooms))
	for _, r := range res.Rooms {
		rooms = append(rooms, RoomInfo{
			SID:             r.Sid,
			Name:            r.Name,
			Metadata:        r.Metadata,
			NumParticipants: int(r.NumParticipants),
		})
	}
	c.JSON(http.StatusOK, ListRoomsResponse{Rooms: rooms})
}

func (h *Handler) createRoom(c *gin.Context) {
	var req CreateRoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "bad_request", "message": err.Error()}})
		return
	}

	room, err := h.RoomClient.CreateRoom(c.Request.Context(), &livekit.CreateRoomRequest{
		Name:     req.Name,
		Metadata: req.Metadata,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
		return
	}

	c.JSON(http.StatusCreated, RoomInfo{
		SID:      room.Sid,
		Name:     room.Name,
		Metadata: room.Metadata,
	})
}

func (h *Handler) deleteRoom(c *gin.Context) {
	name := c.Param("name")
	_, err := h.RoomClient.DeleteRoom(c.Request.Context(), &livekit.DeleteRoomRequest{Room: name})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) listParticipants(c *gin.Context) {
	name := c.Param("name")
	res, err := h.RoomClient.ListParticipants(c.Request.Context(), &livekit.ListParticipantsRequest{Room: name})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
		return
	}

	participants := make([]ParticipantInfo, 0, len(res.Participants))
	for _, p := range res.Participants {
		participants = append(participants, ParticipantInfo{
			SID:      p.Sid,
			Identity: p.Identity,
			Name:     p.Name,
			State:    p.State.String(),
		})
	}
	c.JSON(http.StatusOK, ListParticipantsResponse{Participants: participants})
}

func (h *Handler) removeParticipant(c *gin.Context) {
	name := c.Param("name")
	identity := c.Param("identity")
	_, err := h.RoomClient.RemoveParticipant(c.Request.Context(), &livekit.RoomParticipantIdentity{Room: name, Identity: identity})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
