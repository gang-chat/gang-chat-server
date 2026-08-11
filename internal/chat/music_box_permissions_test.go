package chat

import (
	"net/http"
	"testing"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
)

func TestMusicBoxCapabilityMatrixUsesCurrentRoomRoles(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("music_cap_owner")
	admin := api.register("music_cap_admin")
	peerAdmin := api.register("music_cap_peer_admin")
	member := api.register("music_cap_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Music capabilities"})
	roomID := room["id"].(string)
	adminID := admin.User["id"].(string)
	peerAdminID := peerAdmin.User["id"].(string)
	memberID := member.User["id"].(string)
	if _, err := api.db.Exec(
		`INSERT INTO room_memberships (room_id, user_id, role, joined_at)
		 VALUES (?, ?, 'admin', ?), (?, ?, 'admin', ?), (?, ?, 'member', ?)`,
		roomID, adminID, nowMillis(),
		roomID, peerAdminID, nowMillis(),
		roomID, memberID, nowMillis(),
	); err != nil {
		t.Fatalf("insert music capability members: %v", err)
	}

	temporary := &musicbox.RoomState{ActiveSourceType: musicbox.ActiveSourceTemporary}
	if got := api.chat.musicBoxCapabilities(roomID, memberID, temporary); got.CanControl || got.CanClear || got.CanPlayNow {
		t.Fatalf("ordinary temporary capabilities = %+v", got)
	}
	if got := api.chat.musicBoxCapabilities(roomID, adminID, temporary); !got.CanControl || !got.CanClear || !got.CanPlayNow {
		t.Fatalf("admin temporary capabilities = %+v", got)
	}

	roomPlaylist := &musicbox.RoomState{ActiveSourceType: musicbox.ActiveSourceRoomPlaylist}
	if got := api.chat.musicBoxCapabilities(roomID, memberID, roomPlaylist); !got.CanControl || !got.CanReorder || !got.CanChangeMode {
		t.Fatalf("member room-playlist capabilities = %+v", got)
	}

	memberPlaylist := &musicbox.RoomState{
		ActiveSourceType:      musicbox.ActiveSourceUserPlaylist,
		ActivePlaylistOwnerID: memberID,
	}
	if got := api.chat.musicBoxCapabilities(roomID, memberID, memberPlaylist); !got.CanControl {
		t.Fatalf("personal playlist owner capabilities = %+v", got)
	}
	if got := api.chat.musicBoxCapabilities(roomID, adminID, memberPlaylist); !got.CanControl {
		t.Fatalf("admin over lower-role owner capabilities = %+v", got)
	}

	peerAdminPlaylist := &musicbox.RoomState{
		ActiveSourceType:      musicbox.ActiveSourceUserPlaylist,
		ActivePlaylistOwnerID: peerAdminID,
	}
	if got := api.chat.musicBoxCapabilities(roomID, adminID, peerAdminPlaylist); got.CanControl {
		t.Fatalf("peer admin capabilities must be denied: %+v", got)
	}
	if got := api.chat.musicBoxCapabilities(roomID, "", roomPlaylist); got.CanControl || got.CanEnqueue || got.CanSwitch {
		t.Fatalf("shared SSE capabilities must be conservative: %+v", got)
	}

	memberTemporary := api.chat.musicBoxCapabilities(roomID, memberID, temporary)
	adminTemporary := api.chat.musicBoxCapabilities(roomID, adminID, temporary)
	memberItem := &musicbox.QueueItem{
		AddedByUserID: memberID,
		QueueScope:    musicbox.QueueScopeTemporary,
	}
	foreignItem := &musicbox.QueueItem{
		AddedByUserID: owner.User["id"].(string),
		QueueScope:    musicbox.QueueScopeTemporary,
	}
	if !api.chat.musicBoxCanRemoveItem(memberID, memberItem, memberTemporary) {
		t.Fatal("member must be able to remove their own temporary item")
	}
	if api.chat.musicBoxCanRemoveItem(memberID, foreignItem, memberTemporary) {
		t.Fatal("member must not remove another user's temporary item")
	}
	if !api.chat.musicBoxCanRemoveItem(adminID, foreignItem, adminTemporary) {
		t.Fatal("admin must be able to remove another user's temporary item")
	}
	if switchMusicBoxCommandAllowed("pause", memberTemporary) ||
		!switchMusicBoxCommandAllowed("pause", adminTemporary) ||
		switchMusicBoxCommandAllowed("clear_temporary_playlist", memberTemporary) ||
		!switchMusicBoxCommandAllowed("clear_temporary_playlist", adminTemporary) {
		t.Fatal("music box command matrix did not match temporary capabilities")
	}

	subscription := api.bus.Subscribe(memberID)
	defer subscription.Close()
	subscription.SetRooms([]string{roomID})
	status, response := api.request(
		http.MethodPatch,
		"/rooms/"+roomID+"/members/"+memberID,
		owner.Token,
		map[string]any{"role": "admin"},
	)
	api.requireStatus(status, http.StatusOK, response)
	if got := api.chat.musicBoxCapabilities(roomID, memberID, temporary); !got.CanControl || !got.CanClear {
		t.Fatalf("capabilities did not re-read promoted role: %+v", got)
	}

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event := <-subscription.Events():
			if event.Type == "music_box_changed" {
				return
			}
		case <-timeout.C:
			t.Fatal("role change did not publish a music_box_changed refresh")
		}
	}
}
