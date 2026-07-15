package chat

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAttachmentDispositionUsesUTF8FilenameStar(t *testing.T) {
	header := attachmentDisposition("表情 包.webp")
	if strings.Contains(header, "表情") {
		t.Fatalf("ASCII fallback filename should not contain raw Chinese: %q", header)
	}
	if !strings.Contains(header, `filename="__ _.webp"`) {
		t.Fatalf("header should include ASCII fallback filename: %q", header)
	}
	if !strings.Contains(header, `filename*=UTF-8''%E8%A1%A8%E6%83%85%20%E5%8C%85.webp`) {
		t.Fatalf("header should include RFC 5987 UTF-8 filename: %q", header)
	}
}

func TestStickerAssetReferenceIncludesQuotedPreview(t *testing.T) {
	ids := stickerAssetIDsFromMessagePayload(
		"[]",
		sql.NullString{
			Valid:  true,
			String: `[{"message_id":"source_1","sender_display_name":"Room User","body":"[表情] wave","created_at":"2026-07-14T08:12:00Z","preview_attachment":{"type":"sticker","name":"wave","asset":{"id":"asset_quote_sticker","url":"/assets/asset_quote_sticker/wave.webp","mime_type":"image/webp"}}}]`,
		},
	)
	if len(ids) != 1 || ids[0] != "asset_quote_sticker" {
		t.Fatalf("quoted sticker preview should retain its asset: %v", ids)
	}
}

func TestSaveStickerToPersonalAndRoomPacks(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_owner")
	member := api.register("sticker_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Sticker Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	assetID := "asset_test_sticker"
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'sticker', 'ok.webp', 'image/webp', 12, 'https://example.com/ok.webp', ?)`,
		assetID, owner.User["id"].(string), nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomID,
		"name":    "Room Source",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sourcePack := response["pack"].(map[string]any)
	status, response = api.request(http.MethodPost, "/sticker-packs/"+sourcePack["id"].(string)+"/stickers", owner.Token, map[string]any{
		"asset_id": assetID,
		"name":     "ok",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sourceSticker := response["sticker"].(map[string]any)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "personal",
	})
	api.requireStatus(status, http.StatusCreated, response)
	personalPack := response["pack"].(map[string]any)
	if personalPack["scope"] != "personal" {
		t.Fatalf("member should save to a personal pack: %v", response)
	}
	if personalPack["name"] != defaultPersonalStickerPackName {
		t.Fatalf("member should save to the default personal pack: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "personal",
	})
	api.requireStatus(status, http.StatusCreated, response)
	if !hasStickerNames(response["pack"].(map[string]any), "ok", "ok (2)") {
		t.Fatalf("duplicate personal saved sticker should be suffixed: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "personal",
	})
	api.requireStatus(status, http.StatusCreated, response)
	if !hasStickerNames(response["pack"].(map[string]any), "ok", "ok (2)", "ok (3)") {
		t.Fatalf("third duplicate personal saved sticker should keep incrementing: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", owner.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusCreated, response)
	roomPack := response["pack"].(map[string]any)
	if roomPack["scope"] != "room" || roomPack["room_id"] != roomID {
		t.Fatalf("admin should save to room pack: %v", response)
	}
	if roomPack["name"] != defaultRoomStickerPackName {
		t.Fatalf("admin should save to the default room pack: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", owner.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusCreated, response)
	if !hasStickerNames(response["pack"].(map[string]any), "ok", "ok (2)") {
		t.Fatalf("duplicate room saved sticker should be suffixed: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", owner.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusCreated, response)
	if !hasStickerNames(response["pack"].(map[string]any), "ok", "ok (2)", "ok (3)") {
		t.Fatalf("third duplicate room saved sticker should keep incrementing: %v", response)
	}
}

func TestSaveDeletedStickerFromMessageSnapshot(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_snapshot_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Sticker Snapshot Room", "join_policy": "open"})
	roomID := room["id"].(string)

	assetID := "asset_sticker_snapshot"
	filename := "snapshot.webp"
	body := []byte("sticker snapshot")
	api.putAsset(assetID, filename, "image/webp", body)
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, storage_key, created_at)
		 VALUES (?, ?, 'sticker', ?, 'image/webp', ?, ?, ?, ?)`,
		assetID,
		owner.User["id"].(string),
		filename,
		len(body),
		"/assets/"+assetID+"/"+filename,
		"assets/"+assetID+"/"+filename,
		nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Snapshot Source",
	})
	api.requireStatus(status, http.StatusCreated, response)
	packID := response["pack"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, map[string]any{
		"asset_id": assetID,
		"name":     "snapshot",
	})
	api.requireStatus(status, http.StatusCreated, response)
	stickerID := response["sticker"].(map[string]any)["id"].(string)

	message := api.sendTypedMessage(owner.Token, roomID, "sticker", "[表情] snapshot", []any{
		map[string]any{
			"type":       "sticker",
			"sticker_id": stickerID,
			"name":       "snapshot",
			"asset": map[string]any{
				"id":        assetID,
				"filename":  filename,
				"url":       "/assets/" + assetID + "/" + filename,
				"mime_type": "image/webp",
			},
		},
	})

	status, response = api.request(http.MethodDelete, "/sticker-packs/"+packID+"/stickers/"+stickerID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", owner.Token, map[string]any{
		"sticker_id":        stickerID,
		"source_message_id": message["id"].(string),
		"target_scope":      "personal",
	})
	api.requireStatus(status, http.StatusCreated, response)
	savedPack := response["pack"].(map[string]any)
	if !packHasStickerAsset(savedPack, assetID) {
		t.Fatalf("saved pack should reuse the message asset: %v", savedPack)
	}

	var expiresAt sql.NullInt64
	if err := api.db.QueryRow(
		`SELECT expires_at FROM sticker_asset_lifecycle WHERE asset_id = ?`,
		assetID,
	).Scan(&expiresAt); err != nil {
		t.Fatalf("read sticker lifecycle: %v", err)
	}
	if expiresAt.Valid {
		t.Fatalf("message or pack referenced sticker must not expire: %v", expiresAt.Int64)
	}
}

func TestExpiredOrphanStickerAssetDeletesMetadataAndObject(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_cleanup_owner")
	assetID := "asset_sticker_cleanup"
	filename := "cleanup.webp"
	body := []byte("cleanup")
	api.putAsset(assetID, filename, "image/webp", body)
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, storage_key, created_at)
		 VALUES (?, ?, 'sticker', ?, 'image/webp', ?, ?, ?, ?)`,
		assetID,
		owner.User["id"].(string),
		filename,
		len(body),
		"/assets/"+assetID+"/"+filename,
		"assets/"+assetID+"/"+filename,
		nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	_, err = api.db.Exec(
		`INSERT INTO sticker_asset_lifecycle (asset_id, expires_at, updated_at)
		 VALUES (?, ?, ?)`,
		assetID, nowMillis()-1, nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert lifecycle: %v", err)
	}

	if err := api.chat.cleanupExpiredStickerAssets(context.Background()); err != nil {
		t.Fatalf("cleanup sticker assets: %v", err)
	}
	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id = ?`, assetID).Scan(&count); err != nil {
		t.Fatalf("count cleaned asset: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired orphan metadata should be deleted, count=%d", count)
	}
	if _, err := api.assets.Open(context.Background(), "assets/"+assetID+"/"+filename, assetID, filename); err == nil {
		t.Fatal("expired orphan object should be deleted")
	}
}

func TestConcurrentSaveStickerKeepsDefaultPacksAndNamesUnique(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_concurrent_owner")
	member := api.register("sticker_concurrent_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Sticker Race", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	assetID := "asset_concurrent_sticker"
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'sticker', 'ok.webp', 'image/webp', 12, 'https://example.com/ok.webp', ?)`,
		assetID, owner.User["id"].(string), nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomID,
		"name":    "Room Source",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sourcePack := response["pack"].(map[string]any)
	status, response = api.request(http.MethodPost, "/sticker-packs/"+sourcePack["id"].(string)+"/stickers", owner.Token, map[string]any{
		"asset_id": assetID,
		"name":     "ok",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sourceSticker := response["sticker"].(map[string]any)
	sourceStickerID := sourceSticker["id"].(string)

	saveStickerConcurrently := func(token, scope string) {
		t.Helper()
		const attempts = 6
		var wg sync.WaitGroup
		statuses := make([]int, attempts)
		for index := 0; index < attempts; index++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				rec := postJSON(api.router, "/rooms/"+roomID+"/stickers/save", token, map[string]any{
					"sticker_id":   sourceStickerID,
					"target_scope": scope,
				})
				statuses[index] = rec.Code
			}(index)
		}
		wg.Wait()
		for index, status := range statuses {
			if status != http.StatusCreated {
				t.Fatalf("save %s request %d status = %d, want %d", scope, index, status, http.StatusCreated)
			}
		}
	}

	saveStickerConcurrently(member.Token, "personal")
	memberID := member.User["id"].(string)
	if count := countStickerPacks(t, api.db, "personal", memberID, "", defaultPersonalStickerPackName); count != 1 {
		t.Fatalf("concurrent personal saves should create one default pack, got %d", count)
	}
	personalNames := stickerNamesForDefaultPack(t, api.db, "personal", memberID, "", defaultPersonalStickerPackName)
	expectedNames := []string{"ok (6)", "ok (5)", "ok (4)", "ok (3)", "ok (2)", "ok"}
	if strings.Join(personalNames, "\x00") != strings.Join(expectedNames, "\x00") {
		t.Fatalf("concurrent personal saves should allocate unique names, got %v", personalNames)
	}

	saveStickerConcurrently(owner.Token, "room")
	if count := countStickerPacks(t, api.db, "room", "", roomID, defaultRoomStickerPackName); count != 1 {
		t.Fatalf("concurrent room saves should create one default pack, got %d", count)
	}
	roomNames := stickerNamesForDefaultPack(t, api.db, "room", "", roomID, defaultRoomStickerPackName)
	if strings.Join(roomNames, "\x00") != strings.Join(expectedNames, "\x00") {
		t.Fatalf("concurrent room saves should allocate unique names, got %v", roomNames)
	}
}

func TestConcurrentCreateStickerPacksKeepsNamesUnique(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_pack_concurrent_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Sticker Pack Race", "join_policy": "open"})
	roomID := room["id"].(string)

	createPacksConcurrently := func(scope string) {
		t.Helper()
		const attempts = 6
		var wg sync.WaitGroup
		statuses := make([]int, attempts)
		for index := 0; index < attempts; index++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				body := map[string]any{
					"scope": scope,
					"name":  "same",
				}
				if scope == "room" {
					body["room_id"] = roomID
				}
				rec := postJSON(api.router, "/sticker-packs", owner.Token, body)
				statuses[index] = rec.Code
			}(index)
		}
		wg.Wait()
		for index, status := range statuses {
			if status != http.StatusCreated {
				t.Fatalf("create %s pack request %d status = %d, want %d", scope, index, status, http.StatusCreated)
			}
		}
	}

	createPacksConcurrently("personal")
	ownerID := owner.User["id"].(string)
	personalNames := stickerPackNamesForScope(t, api.db, "personal", ownerID, "")
	expectedNames := []string{"same", "same (2)", "same (3)", "same (4)", "same (5)", "same (6)"}
	if strings.Join(personalNames, "\x00") != strings.Join(expectedNames, "\x00") {
		t.Fatalf("concurrent personal pack creates should allocate unique names, got %v", personalNames)
	}

	createPacksConcurrently("room")
	roomNames := stickerPackNamesForScope(t, api.db, "room", "", roomID)
	if strings.Join(roomNames, "\x00") != strings.Join(expectedNames, "\x00") {
		t.Fatalf("concurrent room pack creates should allocate unique names, got %v", roomNames)
	}
	personalNamesAfterRoomCreates := stickerPackNamesForScope(t, api.db, "personal", ownerID, "")
	if !sameStringSet(personalNamesAfterRoomCreates, personalNames) {
		t.Fatalf(
			"room pack names must not rename existing personal packs: before=%v after=%v",
			personalNames,
			personalNamesAfterRoomCreates,
		)
	}
}

func TestStickerPackDuplicateRenamePreservesOrder(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_pack_order_owner")
	ownerID := owner.User["id"].(string)

	create := func(name string) map[string]any {
		t.Helper()
		status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
			"scope": "personal",
			"name":  name,
		})
		api.requireStatus(status, http.StatusCreated, response)
		return response["pack"].(map[string]any)
	}

	first := create("same")
	second := create("second")
	if first["sort_order"] != float64(10) || second["sort_order"] != float64(20) {
		t.Fatalf("packs should append by default, first=%v second=%v", first, second)
	}

	status, response := api.request(
		http.MethodPatch,
		"/sticker-packs/"+second["id"].(string),
		owner.Token,
		map[string]any{"name": "same"},
	)
	api.requireStatus(status, http.StatusOK, response)
	renamed := response["pack"].(map[string]any)
	if renamed["name"] != "same (2)" || renamed["sort_order"] != float64(20) {
		t.Fatalf("duplicate rename should suffix only the renamed pack without moving it: %v", renamed)
	}

	third := create("same")
	if third["name"] != "same (3)" || third["sort_order"] != float64(30) {
		t.Fatalf("new duplicate pack should receive the next name and append order: %v", third)
	}

	names := stickerPackNamesForScope(t, api.db, "personal", ownerID, "")
	expected := []string{"same", "same (2)", "same (3)"}
	if strings.Join(names, "\x00") != strings.Join(expected, "\x00") {
		t.Fatalf("duplicate naming should preserve existing pack order: got %v want %v", names, expected)
	}
}

func TestListStickerPacksRespectsScopeAndRoom(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_scope_owner")
	roomA := api.createRoom(owner.Token, map[string]any{"name": "Room A", "join_policy": "open"})
	roomB := api.createRoom(owner.Token, map[string]any{"name": "Room B", "join_policy": "open"})
	roomAID := roomA["id"].(string)
	roomBID := roomB["id"].(string)

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Mine",
	})
	api.requireStatus(status, http.StatusCreated, response)

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomAID,
		"name":    "Room A Pack",
	})
	api.requireStatus(status, http.StatusCreated, response)
	roomAPackID := response["pack"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomBID,
		"name":    "Room B Pack",
	})
	api.requireStatus(status, http.StatusCreated, response)

	status, response = api.request(http.MethodGet, "/sticker-packs?scope=room&room_id="+roomAID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	roomPacks := response["packs"].([]any)
	if len(roomPacks) != 1 {
		t.Fatalf("room list should include only that room's packs: %v", response)
	}
	roomPack := roomPacks[0].(map[string]any)
	if roomPack["id"] != roomAPackID || roomPack["scope"] != "room" || roomPack["room_id"] != roomAID {
		t.Fatalf("room list returned wrong pack: %v", roomPack)
	}

	status, response = api.request(http.MethodGet, "/sticker-packs?scope=personal", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	personalPacks := response["packs"].([]any)
	if len(personalPacks) != 1 || personalPacks[0].(map[string]any)["scope"] != "personal" {
		t.Fatalf("personal list should include only personal packs: %v", response)
	}
}

func TestSuperuserManagesTargetPersonalStickerPacks(t *testing.T) {
	api := newAPIHarness(t)
	admin := api.register("sticker_target_admin")
	target := api.register("sticker_target_user")
	other := api.register("sticker_target_other")
	adminID := admin.User["id"].(string)
	targetID := target.User["id"].(string)
	if _, err := api.db.Exec(`UPDATE users SET is_superuser = 1 WHERE id = ?`, adminID); err != nil {
		t.Fatalf("make test user superuser: %v", err)
	}

	status, response := api.request(http.MethodPost, "/sticker-packs", other.Token, map[string]any{
		"scope":         "personal",
		"owner_user_id": targetID,
		"name":          "Denied",
	})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPost, "/sticker-packs", admin.Token, map[string]any{
		"scope":         "personal",
		"owner_user_id": targetID,
		"name":          "Target Pack",
	})
	api.requireStatus(status, http.StatusCreated, response)
	packID := response["pack"].(map[string]any)["id"].(string)

	var storedOwner string
	if err := api.db.QueryRow(`SELECT owner_user_id FROM sticker_packs WHERE id = ?`, packID).Scan(&storedOwner); err != nil {
		t.Fatalf("read target pack owner: %v", err)
	}
	if storedOwner != targetID {
		t.Fatalf("target pack owner = %q, want %q", storedOwner, targetID)
	}

	status, response = api.request(
		http.MethodGet,
		"/sticker-packs?scope=personal&owner_user_id="+url.QueryEscape(targetID),
		admin.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	packs := response["packs"].([]any)
	if len(packs) != 1 || packs[0].(map[string]any)["id"] != packID {
		t.Fatalf("superuser target list returned wrong packs: %v", response)
	}

	assetID := "asset_target_managed_sticker"
	filename := "target.webp"
	body := []byte("target-sticker")
	api.putAsset(assetID, filename, "image/webp", body)
	if _, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'sticker', ?, 'image/webp', ?, ?, ?)`,
		assetID, adminID, filename, len(body), "/assets/"+assetID+"/"+filename, nowMillis(),
	); err != nil {
		t.Fatalf("insert target managed asset: %v", err)
	}
	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers", admin.Token, map[string]any{
		"asset_id": assetID,
		"name":     "target",
	})
	api.requireStatus(status, http.StatusCreated, response)
	stickerID := response["sticker"].(map[string]any)["id"].(string)

	download := api.rawRequest(http.MethodGet, "/stickers/download?ids="+stickerID, admin.Token, nil, nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), body) {
		t.Fatalf("superuser target sticker download status=%d body=%q", download.Code, download.Body.String())
	}

	status, response = api.request(http.MethodGet, "/sticker-packs?scope=personal", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	targetPacks := response["packs"].([]any)
	if len(targetPacks) != 1 || targetPacks[0].(map[string]any)["id"] != packID {
		t.Fatalf("target user should see superuser-managed pack: %v", response)
	}
}

func TestSuperuserReadsTargetAccountSessions(t *testing.T) {
	api := newAPIHarness(t)
	admin := api.register("session_target_admin")
	target := api.register("session_target_user")
	other := api.register("session_target_other")
	adminID := admin.User["id"].(string)
	targetID := target.User["id"].(string)
	if _, err := api.db.Exec(`UPDATE users SET is_superuser = 1 WHERE id = ?`, adminID); err != nil {
		t.Fatalf("make test user superuser: %v", err)
	}

	denied := api.rawRequest(
		http.MethodGet,
		"/users/"+targetID+"/sessions",
		other.Token,
		nil,
		nil,
	)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("ordinary user target sessions status=%d body=%q", denied.Code, denied.Body.String())
	}

	allowed := api.rawRequest(
		http.MethodGet,
		"/users/"+targetID+"/sessions",
		admin.Token,
		nil,
		nil,
	)
	if allowed.Code != http.StatusOK {
		t.Fatalf("superuser target sessions status=%d body=%q", allowed.Code, allowed.Body.String())
	}
	var sessions []map[string]any
	if err := json.Unmarshal(allowed.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode target sessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatalf("target registration session missing: %s", allowed.Body.String())
	}
	if sessions[0]["is_current"] != false {
		t.Fatalf("target session must not be marked as the admin's current session: %v", sessions[0])
	}
	if _, ok := sessions[0]["ip_address"]; !ok {
		t.Fatalf("target session should expose its cloud IP field: %v", sessions[0])
	}
}

func TestSuperuserDeletesTargetAccountThroughSharedRetentionPath(t *testing.T) {
	api := newAPIHarness(t)
	admin := api.register("delete_target_admin")
	target := api.register("delete_target_user")
	other := api.register("delete_target_other")
	adminID := admin.User["id"].(string)
	targetID := target.User["id"].(string)
	if _, err := api.db.Exec(`UPDATE users SET is_superuser = 1 WHERE id = ?`, adminID); err != nil {
		t.Fatalf("make test user superuser: %v", err)
	}

	status, response := api.request(
		http.MethodDelete,
		"/users/"+targetID+"/account",
		other.Token,
		map[string]any{"confirm": true},
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodDelete,
		"/users/"+adminID+"/account",
		admin.Token,
		map[string]any{"confirm": true},
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodDelete,
		"/users/"+targetID+"/account",
		admin.Token,
		map[string]any{"confirm": true},
	)
	api.requireStatus(status, http.StatusOK, response)

	var remaining int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, targetID).Scan(&remaining); err != nil {
		t.Fatalf("count deleted target user: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("target user still exists after confirmed superuser deletion")
	}
}

func TestAddStickerIsIdempotent(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_idempotent_owner")

	assetID := "asset_idempotent_sticker"
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'sticker', 'hi.webp', 'image/webp', 12, 'https://example.com/hi.webp', ?)`,
		assetID, owner.User["id"].(string), nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Idempotent Stickers",
	})
	api.requireStatus(status, http.StatusCreated, response)
	pack := response["pack"].(map[string]any)
	packID := pack["id"].(string)

	body := map[string]any{
		"asset_id": assetID,
		"name":     "hi",
	}
	headers := map[string]string{"Idempotency-Key": "test-add-sticker-key"}
	status, response = api.requestWithHeaders(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, body, headers)
	api.requireStatus(status, http.StatusCreated, response)
	firstSticker := response["sticker"].(map[string]any)

	status, response = api.requestWithHeaders(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, body, headers)
	api.requireStatus(status, http.StatusCreated, response)
	secondSticker := response["sticker"].(map[string]any)
	if secondSticker["id"] != firstSticker["id"] {
		t.Fatalf("expected idempotent replay to return same sticker: first=%v second=%v", firstSticker, secondSticker)
	}

	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM stickers WHERE pack_id = ? AND asset_id = ?`, packID, assetID).Scan(&count); err != nil {
		t.Fatalf("count stickers: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one sticker row, got %d", count)
	}
}

func TestManageStickersRenameReorderAndDownload(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_manage_owner")
	other := api.register("sticker_manage_other")

	assets := []struct {
		id       string
		filename string
		mimeType string
		body     []byte
	}{
		{id: "asset_manage_one", filename: "one.webp", mimeType: "image/webp", body: []byte("one-image")},
		{id: "asset_manage_two", filename: "two.png", mimeType: "image/png", body: []byte("two-image")},
	}
	for _, asset := range assets {
		api.putAsset(asset.id, asset.filename, asset.mimeType, asset.body)
		_, err := api.db.Exec(
			`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
			 VALUES (?, ?, 'sticker', ?, ?, ?, ?, ?)`,
			asset.id, owner.User["id"].(string), asset.filename, asset.mimeType, len(asset.body), "/assets/"+asset.id+"/"+asset.filename, nowMillis(),
		)
		if err != nil {
			t.Fatalf("insert asset: %v", err)
		}
	}

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Managed Stickers",
	})
	api.requireStatus(status, http.StatusCreated, response)
	packID := response["pack"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, map[string]any{
		"asset_id": assets[0].id,
		"name":     "smile",
	})
	api.requireStatus(status, http.StatusCreated, response)
	first := response["sticker"].(map[string]any)
	firstID := first["id"].(string)
	if first["name"] != "smile" {
		t.Fatalf("first sticker name mismatch: %v", first)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, map[string]any{
		"asset_id": assets[1].id,
		"name":     "smile",
	})
	api.requireStatus(status, http.StatusCreated, response)
	second := response["sticker"].(map[string]any)
	secondID := second["id"].(string)
	if second["name"] != "smile (2)" {
		t.Fatalf("duplicate sticker name should be suffixed: %v", second)
	}

	status, response = api.request(http.MethodPatch, "/sticker-packs/"+packID+"/stickers/"+secondID, owner.Token, map[string]any{
		"name": "smile",
	})
	api.requireStatus(status, http.StatusOK, response)
	renamed := response["sticker"].(map[string]any)
	if renamed["name"] != "smile (2)" {
		t.Fatalf("rename should preserve unique name: %v", renamed)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers/reorder", owner.Token, map[string]any{
		"sticker_ids": []string{secondID, firstID},
	})
	api.requireStatus(status, http.StatusOK, response)
	reordered := response["pack"].(map[string]any)["stickers"].([]any)
	if reordered[0].(map[string]any)["id"] != secondID || reordered[1].(map[string]any)["id"] != firstID {
		t.Fatalf("stickers not reordered: %v", reordered)
	}

	single := api.rawRequest(http.MethodGet, "/stickers/download?ids="+firstID, owner.Token, nil, nil)
	if single.Code != http.StatusOK {
		t.Fatalf("single download status=%d body=%q", single.Code, single.Body.String())
	}
	if single.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("single download content type mismatch: %s", single.Header().Get("Content-Type"))
	}
	if !bytes.Equal(single.Body.Bytes(), assets[0].body) {
		t.Fatalf("single download body mismatch: %q", single.Body.Bytes())
	}

	denied := api.rawRequest(http.MethodGet, "/stickers/download?ids="+firstID, other.Token, nil, nil)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("other user should not download personal sticker: status=%d body=%q", denied.Code, denied.Body.String())
	}

	batch := api.rawRequest(http.MethodGet, "/stickers/download?ids="+secondID+","+firstID, owner.Token, nil, nil)
	if batch.Code != http.StatusOK {
		t.Fatalf("batch download status=%d body=%q", batch.Code, batch.Body.String())
	}
	if batch.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("batch download content type mismatch: %s", batch.Header().Get("Content-Type"))
	}
	archive, err := zip.NewReader(bytes.NewReader(batch.Body.Bytes()), int64(batch.Body.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if len(archive.File) != 2 {
		t.Fatalf("zip should contain two files: %v", archive.File)
	}
	if archive.File[0].Name != "smile (2).png" || archive.File[1].Name != "smile.webp" {
		t.Fatalf("zip entry names should follow selected order and sticker names: %v, %v", archive.File[0].Name, archive.File[1].Name)
	}

	status, response = api.request(http.MethodDelete, "/sticker-packs/"+packID+"/stickers/"+firstID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodDelete, "/sticker-packs/"+packID+"/stickers/"+firstID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	var remaining int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM stickers WHERE id = ?`, firstID).Scan(&remaining); err != nil {
		t.Fatalf("count deleted sticker: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("deleted sticker still exists")
	}
}

func TestUploadImageStoresAssetFile(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("asset_owner")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "avatar"); err != nil {
		t.Fatalf("write purpose: %v", err)
	}
	part, err := writer.CreateFormFile("file", "avatar.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if _, err := part.Write(pngBytes); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/images", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+owner.Token)
	rec := httptest.NewRecorder()
	api.router.ServeHTTP(rec, req)

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	api.requireStatus(rec.Code, http.StatusCreated, response)
	asset := response["asset"].(map[string]any)
	if asset["mime_type"] != "image/png" {
		t.Fatalf("expected sniffed image/png mime: %v", asset)
	}
	assetID := asset["id"].(string)
	var filename string
	if err := api.db.QueryRow(`SELECT filename FROM assets WHERE id = ?`, assetID).Scan(&filename); err != nil {
		t.Fatalf("read asset row: %v", err)
	}
	saved := api.readAsset(assetID, filename)
	if !bytes.Equal(saved, pngBytes) {
		t.Fatalf("saved asset bytes changed: %v", saved)
	}
}

func TestUploadFileStoresAssetFile(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("file_asset_owner")

	fileBytes := []byte("%PDF-1.7\nhello")
	status, response := api.uploadMultipart("/uploads/files", owner.Token, "../report 2026?.pdf", "application/pdf", "", fileBytes)
	api.requireStatus(status, http.StatusCreated, response)

	asset := response["asset"].(map[string]any)
	assetID := asset["id"].(string)
	if asset["filename"] != "report-2026.pdf" {
		t.Fatalf("filename should be sanitized: %v", asset)
	}
	if asset["mime_type"] != "application/pdf" {
		t.Fatalf("expected sniffed application/pdf mime: %v", asset)
	}
	if int64(asset["size_bytes"].(float64)) != int64(len(fileBytes)) {
		t.Fatalf("size_bytes mismatch: %v", asset)
	}
	if asset["thumbnail_url"] != nil {
		t.Fatalf("non-image upload should not expose thumbnail_url: %v", asset)
	}

	var purpose, filename, storageKey string
	if err := api.db.QueryRow(`SELECT purpose, filename, storage_key FROM assets WHERE id = ?`, assetID).Scan(&purpose, &filename, &storageKey); err != nil {
		t.Fatalf("read asset row: %v", err)
	}
	if purpose != "message_file" {
		t.Fatalf("default file purpose mismatch: %q", purpose)
	}
	if storageKey != "assets/"+assetID+"/"+filename {
		t.Fatalf("storage key mismatch: %q", storageKey)
	}
	saved := api.readAsset(assetID, filename)
	if !bytes.Equal(saved, fileBytes) {
		t.Fatalf("saved asset bytes changed: %v", saved)
	}
}

func TestAssetRouteSendsExpiringCacheValidators(t *testing.T) {
	api := newAPIHarness(t)
	RegisterAssetRoutes(api.router, api.db, api.cfg, api.assets)

	owner := api.register("asset_cache_owner")
	assetID := "asset_cache_route"
	filename := "report.txt"
	createdAt := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC).UnixMilli()
	body := []byte("cached asset")
	api.putAsset(assetID, filename, "text/plain", body)
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'message_file', ?, 'text/plain', ?, ?, ?)`,
		assetID, owner.User["id"].(string), filename, len(body), "/assets/"+assetID+"/"+filename, createdAt,
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/"+assetID+"/"+filename, nil)
	api.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, body) {
		t.Fatalf("asset body mismatch: %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
	if rec.Header().Get("Expires") == "" {
		t.Fatalf("asset response missing Expires header")
	}
	if got := rec.Header().Get("Last-Modified"); got != "Thu, 18 Jun 2026 10:00:00 GMT" {
		t.Fatalf("unexpected Last-Modified: %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("asset response missing ETag")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/assets/"+assetID+"/"+filename, nil)
	req.Header.Set("If-None-Match", etag)
	api.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("unexpected conditional status: %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 response should not include a body: %q", rec.Body.String())
	}
}

func TestUploadImageRejectsNonImage(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("non_image_owner")

	status, response := api.uploadMultipart("/uploads/images", owner.Token, "notes.txt", "text/plain", "", []byte("not an image"))
	api.requireStatus(status, http.StatusBadRequest, response)

	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&count); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 0 {
		t.Fatalf("non-image upload should not create asset rows, got %d", count)
	}
}

func TestUploadFileRejectsOverLimit(t *testing.T) {
	api := newAPIHarness(t)
	api.cfg.AssetUploadMaxBytes = 4
	owner := api.register("file_limit_owner")

	status, response := api.uploadMultipart("/uploads/files", owner.Token, "tiny.bin", "application/octet-stream", "", []byte("12345"))
	api.requireStatus(status, http.StatusRequestEntityTooLarge, response)

	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&count); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 0 {
		t.Fatalf("over-limit upload should not create asset rows, got %d", count)
	}
}
