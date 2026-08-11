package chat

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
)

func TestPublishMusicBoxProgressUsesCompactRoomScopedPayload(t *testing.T) {
	bus := eventbus.New()
	sub := bus.Subscribe("listener")
	sub.SetRooms([]string{"room-1"})
	defer sub.Close()

	h := &Handler{Bus: bus}
	h.publishMusicBoxProgress("room-1", musicbox.ProgressSnapshot{
		Revision:      12,
		CurrentItemID: "item-12",
		PositionMS:    3456,
	})

	select {
	case event := <-sub.Events():
		if event.Type != "music_box_progress" || event.RoomID != "room-1" {
			t.Fatalf("unexpected progress event: %+v", event)
		}
		payload, ok := event.Data.(gin.H)
		if !ok {
			t.Fatalf("unexpected payload type %T", event.Data)
		}
		if payload["revision"] != int64(12) ||
			payload["current_item_id"] != "item-12" ||
			payload["position_ms"] != int64(3456) {
			t.Fatalf("unexpected progress payload: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for compact music progress event")
	}
}

func TestPublishMusicBoxProgressSkipsEmptyCurrentItem(t *testing.T) {
	bus := eventbus.New()
	sub := bus.Subscribe("listener")
	sub.SetRooms([]string{"room-1"})
	defer sub.Close()

	h := &Handler{Bus: bus}
	h.publishMusicBoxProgress("room-1", musicbox.ProgressSnapshot{Revision: 12})

	select {
	case event := <-sub.Events():
		t.Fatalf("unexpected progress event: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}
