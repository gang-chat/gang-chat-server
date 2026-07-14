package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"testing"
)

type apiHarness struct {
	t                 *testing.T
	router            *gin.Engine
	db                *sql.DB
	live              *fakeLiveController
	bus               *eventbus.Bus
	assets            *storage.AssetStorage
	chat              *Handler
	cfg               *config.Config
	verificationEmail *fakeVerificationEmailSender
}

type sentPasswordResetEmail struct {
	to   string
	code string
}

type fakeVerificationEmailSender struct {
	sent             []sentPasswordResetEmail
	registrationSent []sentPasswordResetEmail
}

func (f *fakeVerificationEmailSender) SendPasswordResetCode(_ context.Context, to, code string) error {
	f.sent = append(f.sent, sentPasswordResetEmail{to: to, code: code})
	return nil
}

func (f *fakeVerificationEmailSender) SendRegistrationVerificationCode(_ context.Context, to, code string) error {
	f.registrationSent = append(f.registrationSent, sentPasswordResetEmail{to: to, code: code})
	return nil
}

// fakeLiveController records the LiveKit media-control calls moderation makes,
// so tests can assert the server drove the media session (not just the DB).
type fakeLiveController struct {
	removed          []string // "room/identity"
	mediaPermissions []string // "room/identity publish=true|false subscribe=true|false"
	publishSet       []string // "room/identity=true|false"
	subscribeSet     []string // "room/identity=true|false"
	micMuted         []string // "room/identity=true|false"
	removeErr        error
}

func (f *fakeLiveController) RemoveParticipant(room, identity string) error {
	f.removed = append(f.removed, room+"/"+identity)
	return f.removeErr
}

func (f *fakeLiveController) SetCanPublish(room, identity string, canPublish bool) error {
	f.publishSet = append(f.publishSet, room+"/"+identity+"="+strconv.FormatBool(canPublish))
	return nil
}

func (f *fakeLiveController) SetMediaPermissions(room, identity string, canPublish, canSubscribe bool) error {
	f.mediaPermissions = append(
		f.mediaPermissions,
		room+"/"+identity+" publish="+strconv.FormatBool(canPublish)+" subscribe="+strconv.FormatBool(canSubscribe),
	)
	return nil
}

func (f *fakeLiveController) SetCanSubscribe(room, identity string, canSubscribe bool) error {
	f.subscribeSet = append(f.subscribeSet, room+"/"+identity+"="+strconv.FormatBool(canSubscribe))
	return nil
}

func (f *fakeLiveController) MuteMicrophone(room, identity string, muted bool) error {
	f.micMuted = append(f.micMuted, room+"/"+identity+"="+strconv.FormatBool(muted))
	return nil
}

type testSession struct {
	Token string
	User  map[string]any
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		JWTSecret:              "test-secret",
		AccessTokenTTLSeconds:  900,
		RefreshTokenTTLSeconds: 2592000,
		LoginMaxAttempts:       5,
		LoginWindowSeconds:     900,
		LiveKitHost:            "http://localhost:7880",
	}
	dsn := os.Getenv("GANG_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GANG_TEST_MYSQL_DSN is required for MySQL-backed chat API tests")
	}
	pool := db.Connect(dsn)
	t.Cleanup(func() { _ = pool.Close() })
	prepareChatMySQLTestDatabase(t, pool)

	router := gin.New()
	api := router.Group("/api/v1")
	authHandler := auth.RegisterRoutes(api, pool, cfg)
	verificationEmail := &fakeVerificationEmailSender{}
	authHandler.VerificationEmailSender = verificationEmail

	authMW := &auth.AuthMiddleware{DB: pool, JWTSecret: cfg.JWTSecret}
	chatGroup := api.Group("")
	chatGroup.Use(authMW.Handle)
	live := &fakeLiveController{}
	bus := eventbus.New()
	assetStore := storage.NewMemoryAssetStorage()
	chatHandler := RegisterRoutes(chatGroup, pool, cfg, bus, live, nil, assetStore)

	return &apiHarness{
		t:                 t,
		router:            router,
		db:                pool,
		live:              live,
		bus:               bus,
		assets:            assetStore,
		chat:              chatHandler,
		cfg:               cfg,
		verificationEmail: verificationEmail,
	}
}

func (h *apiHarness) putAsset(id, filename, mimeType string, body []byte) {
	h.t.Helper()
	tmp, err := os.CreateTemp("", "gang-chat-test-asset-*")
	if err != nil {
		h.t.Fatalf("create temp asset: %v", err)
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		h.t.Fatalf("write temp asset: %v", err)
	}
	if err := tmp.Close(); err != nil {
		h.t.Fatalf("close temp asset: %v", err)
	}
	if err := h.assets.PutFile(context.Background(), h.assets.ObjectKey(id, filename), path, mimeType); err != nil {
		h.t.Fatalf("put asset: %v", err)
	}
}

func (h *apiHarness) readAsset(id, filename string) []byte {
	h.t.Helper()
	body, err := h.assets.Open(context.Background(), h.assets.ObjectKey(id, filename), id, filename)
	if err != nil {
		h.t.Fatalf("open asset: %v", err)
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		h.t.Fatalf("read asset: %v", err)
	}
	return raw
}

func (h *apiHarness) request(method, path, token string, body any) (int, map[string]any) {
	return h.requestWithHeaders(method, path, token, body, nil)
}

func (h *apiHarness) requestWithHeaders(method, path, token string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	rec := h.rawRequest(method, path, token, body, headers)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			h.t.Fatalf("%s %s returned invalid JSON: %v; body=%q", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, decoded
}

func (h *apiHarness) rawRequest(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *apiHarness) requireStatus(status, want int, response map[string]any) {
	h.t.Helper()
	if status != want {
		h.t.Fatalf("unexpected status: got %d want %d response=%v", status, want, response)
	}
}

func (h *apiHarness) register(username string) testSession {
	h.t.Helper()
	email := username + "@example.com"
	status, response := h.request(http.MethodPost, "/auth/email-verification/inspect", "", map[string]any{
		"email": email,
	})
	h.requireStatus(status, http.StatusOK, response)
	status, response = h.request(http.MethodPost, "/auth/email-verification/start", "", map[string]any{
		"email": email,
	})
	h.requireStatus(status, http.StatusOK, response)
	challengeID, _ := response["challenge_id"].(string)
	if challengeID == "" || len(h.verificationEmail.registrationSent) == 0 {
		h.t.Fatalf("email verification did not send a code: %v", response)
	}
	code := h.verificationEmail.registrationSent[len(h.verificationEmail.registrationSent)-1].code
	status, response = h.request(http.MethodPost, "/auth/email-verification/verify", "", map[string]any{
		"challenge_id": challengeID,
		"code":         code,
	})
	h.requireStatus(status, http.StatusOK, response)
	verificationToken, _ := response["verification_token"].(string)
	if verificationToken == "" {
		h.t.Fatalf("email verification response missing token: %v", response)
	}
	status, response = h.request(http.MethodPost, "/auth/register", "", map[string]any{
		"username":                 username,
		"email":                    email,
		"password":                 "correct horse battery staple",
		"email_verification_token": verificationToken,
	})
	h.requireStatus(status, http.StatusCreated, response)
	user, ok := response["user"].(map[string]any)
	if !ok {
		h.t.Fatalf("register response missing user: %v", response)
	}
	token, ok := response["access_token"].(string)
	if !ok || token == "" {
		h.t.Fatalf("register response missing access token: %v", response)
	}
	return testSession{Token: token, User: user}
}

func (h *apiHarness) verifyEmail(email string) string {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/auth/email-verification/inspect", "", map[string]any{
		"email": email,
	})
	h.requireStatus(status, http.StatusOK, response)
	status, response = h.request(http.MethodPost, "/auth/email-verification/start", "", map[string]any{
		"email": email,
	})
	h.requireStatus(status, http.StatusOK, response)
	challengeID, _ := response["challenge_id"].(string)
	if challengeID == "" || len(h.verificationEmail.registrationSent) == 0 {
		h.t.Fatalf("email verification did not send a code: %v", response)
	}
	code := h.verificationEmail.registrationSent[len(h.verificationEmail.registrationSent)-1].code
	status, response = h.request(http.MethodPost, "/auth/email-verification/verify", "", map[string]any{
		"challenge_id": challengeID,
		"code":         code,
	})
	h.requireStatus(status, http.StatusOK, response)
	token, _ := response["verification_token"].(string)
	if token == "" {
		h.t.Fatalf("email verification response missing token: %v", response)
	}
	return token
}

func (h *apiHarness) login(login, password string) testSession {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/auth/login", "", map[string]any{
		"login":    login,
		"password": password,
	})
	h.requireStatus(status, http.StatusOK, response)
	user, ok := response["user"].(map[string]any)
	if !ok {
		h.t.Fatalf("login response missing user: %v", response)
	}
	token, ok := response["access_token"].(string)
	if !ok || token == "" {
		h.t.Fatalf("login response missing access token: %v", response)
	}
	return testSession{Token: token, User: user}
}

func (h *apiHarness) createRoom(token string, body map[string]any) map[string]any {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/rooms", token, body)
	h.requireStatus(status, http.StatusCreated, response)
	room, ok := response["room"].(map[string]any)
	if !ok {
		h.t.Fatalf("create room response missing room: %v", response)
	}
	return room
}

func (h *apiHarness) uploadMultipart(path, token, filename, contentType, purpose string, data []byte) (int, map[string]any) {
	h.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if purpose != "" {
		if err := writer.WriteField("purpose", purpose); err != nil {
			h.t.Fatalf("write purpose: %v", err)
		}
	}
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", `form-data; name="file"; filename="`+strings.ReplaceAll(filename, `"`, `\"`)+`"`)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	part, err := writer.CreatePart(headers)
	if err != nil {
		h.t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		h.t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		h.t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			h.t.Fatalf("%s returned invalid JSON: %v; body=%q", path, err, rec.Body.String())
		}
	}
	return rec.Code, decoded
}

func memberByUserID(t *testing.T, response map[string]any, userID string) map[string]any {
	t.Helper()
	items, ok := response["members"].([]any)
	if !ok {
		t.Fatalf("members response missing members: %v", response)
	}
	for _, item := range items {
		member, ok := item.(map[string]any)
		if !ok {
			continue
		}
		user, ok := member["user"].(map[string]any)
		if ok && user["id"] == userID {
			return member
		}
	}
	t.Fatalf("member %s not found: %v", userID, response)
	return nil
}

func hasStickerNames(pack map[string]any, names ...string) bool {
	stickers, ok := pack["stickers"].([]any)
	if !ok {
		return false
	}
	seen := make(map[string]int, len(stickers))
	for _, item := range stickers {
		sticker, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := sticker["name"].(string)
		seen[name]++
	}
	for _, name := range names {
		if seen[name] == 0 {
			return false
		}
		seen[name]--
	}
	return true
}

func packHasStickerAsset(pack map[string]any, assetID string) bool {
	stickers, ok := pack["stickers"].([]any)
	if !ok {
		return false
	}
	for _, item := range stickers {
		sticker, ok := item.(map[string]any)
		if !ok {
			continue
		}
		asset, ok := sticker["asset"].(map[string]any)
		if ok && asset["id"] == assetID {
			return true
		}
	}
	return false
}

func postJSON(router *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	payload, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func countStickerPacks(t *testing.T, db *sql.DB, scope, ownerUserID, roomID, name string) int {
	t.Helper()
	var count int
	var err error
	if scope == "personal" {
		err = db.QueryRow(
			`SELECT COUNT(*) FROM sticker_packs WHERE scope = 'personal' AND owner_user_id = ? AND name = ?`,
			ownerUserID, name,
		).Scan(&count)
	} else {
		err = db.QueryRow(
			`SELECT COUNT(*) FROM sticker_packs WHERE scope = 'room' AND room_id = ? AND name = ?`,
			roomID, name,
		).Scan(&count)
	}
	if err != nil {
		t.Fatalf("count sticker packs: %v", err)
	}
	return count
}

func stickerNamesForDefaultPack(t *testing.T, db *sql.DB, scope, ownerUserID, roomID, name string) []string {
	t.Helper()
	var rows *sql.Rows
	var err error
	if scope == "personal" {
		rows, err = db.Query(
			`SELECT s.name
			 FROM stickers s
			 JOIN sticker_packs p ON p.id = s.pack_id
			 WHERE p.scope = 'personal' AND p.owner_user_id = ? AND p.name = ?
			 ORDER BY s.sort_order ASC, s.created_at ASC, s.id ASC`,
			ownerUserID, name,
		)
	} else {
		rows, err = db.Query(
			`SELECT s.name
			 FROM stickers s
			 JOIN sticker_packs p ON p.id = s.pack_id
			 WHERE p.scope = 'room' AND p.room_id = ? AND p.name = ?
			 ORDER BY s.sort_order ASC, s.created_at ASC, s.id ASC`,
			roomID, name,
		)
	}
	if err != nil {
		t.Fatalf("query sticker names: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var stickerName string
		if err := rows.Scan(&stickerName); err != nil {
			t.Fatalf("scan sticker name: %v", err)
		}
		names = append(names, stickerName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sticker names: %v", err)
	}
	return names
}

func stickerPackNamesForScope(t *testing.T, db *sql.DB, scope, ownerUserID, roomID string) []string {
	t.Helper()
	var rows *sql.Rows
	var err error
	if scope == "personal" {
		rows, err = db.Query(
			`SELECT name FROM sticker_packs
			 WHERE scope = 'personal' AND owner_user_id = ?
			 ORDER BY sort_order ASC, created_at ASC, id ASC`,
			ownerUserID,
		)
	} else {
		rows, err = db.Query(
			`SELECT name FROM sticker_packs
			 WHERE scope = 'room' AND room_id = ?
			 ORDER BY sort_order ASC, created_at ASC, id ASC`,
			roomID,
		)
	}
	if err != nil {
		t.Fatalf("query sticker pack names: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var packName string
		if err := rows.Scan(&packName); err != nil {
			t.Fatalf("scan sticker pack name: %v", err)
		}
		names = append(names, packName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sticker pack names: %v", err)
	}
	return names
}

func sameStringSet(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	counts := make(map[string]int, len(actual))
	for _, value := range actual {
		counts[value]++
	}
	for _, value := range expected {
		if counts[value] == 0 {
			return false
		}
		counts[value]--
	}
	return true
}

func participantByUserID(t *testing.T, live map[string]any, userID string) map[string]any {
	t.Helper()
	items, ok := live["participants"].([]any)
	if !ok {
		t.Fatalf("live response missing participants: %v", live)
	}
	for _, item := range items {
		participant, ok := item.(map[string]any)
		if !ok {
			continue
		}
		user, ok := participant["user"].(map[string]any)
		if ok && user["id"] == userID {
			return participant
		}
	}
	t.Fatalf("participant %s not found: %v", userID, live)
	return nil
}

func (h *apiHarness) sendMessage(token, roomID, body string) map[string]any {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/rooms/"+roomID+"/messages", token, map[string]any{
		"client_message_id": "test_msg_" + idgen.New("client"),
		"body":              body,
	})
	h.requireStatus(status, http.StatusCreated, response)
	message, ok := response["message"].(map[string]any)
	if !ok {
		h.t.Fatalf("send message response missing message: %v", response)
	}
	return message
}

func (h *apiHarness) sendTypedMessage(token, roomID, messageType, body string, attachments []any) map[string]any {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/rooms/"+roomID+"/messages", token, map[string]any{
		"client_message_id": "test_msg_" + idgen.New("client"),
		"type":              messageType,
		"body":              body,
		"attachments":       attachments,
	})
	h.requireStatus(status, http.StatusCreated, response)
	message, ok := response["message"].(map[string]any)
	if !ok {
		h.t.Fatalf("send typed message response missing message: %v", response)
	}
	return message
}

func parseNumericID(t *testing.T, value any) int64 {
	t.Helper()
	raw, ok := value.(string)
	if !ok || raw == "" {
		t.Fatalf("expected numeric id string, got %#v", value)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("numeric id is not parseable: %q", raw)
	}
	return n
}

func assertRoomInvitesKeepDeletedRooms(t *testing.T, pool *sql.DB) {
	t.Helper()
	rows, err := pool.Query(`
		SELECT kcu.REFERENCED_TABLE_NAME, kcu.COLUMN_NAME, rc.DELETE_RULE
		FROM information_schema.KEY_COLUMN_USAGE kcu
		JOIN information_schema.REFERENTIAL_CONSTRAINTS rc
		  ON rc.CONSTRAINT_SCHEMA = kcu.CONSTRAINT_SCHEMA
		 AND rc.CONSTRAINT_NAME = kcu.CONSTRAINT_NAME
		 AND rc.TABLE_NAME = kcu.TABLE_NAME
		WHERE kcu.TABLE_SCHEMA = DATABASE()
		  AND kcu.TABLE_NAME = 'room_invites'
		  AND kcu.REFERENCED_TABLE_NAME IS NOT NULL`)
	if err != nil {
		t.Fatalf("read room_invites foreign keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tableName, from, onDelete string
		if err := rows.Scan(&tableName, &from, &onDelete); err != nil {
			t.Fatalf("scan room_invites foreign key: %v", err)
		}
		if tableName == "rooms" && from == "room_id" {
			t.Fatalf("room_invites.room_id must not cascade with rooms, on_delete=%s", onDelete)
		}
		if tableName == "users" && from == "inviter_user_id" {
			t.Fatalf("room_invites.inviter_user_id must not cascade with users, on_delete=%s", onDelete)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate room_invites foreign keys: %v", err)
	}
}

func findMessage(t *testing.T, response map[string]any, messageID string) map[string]any {
	t.Helper()
	items, ok := response["messages"].([]any)
	if !ok {
		t.Fatalf("messages response missing messages: %v", response)
	}
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if ok && msg["id"] == messageID {
			return msg
		}
	}
	t.Fatalf("message %s not found in response: %v", messageID, response)
	return nil
}

func roomCardByID(t *testing.T, response map[string]any, roomID string) map[string]any {
	t.Helper()
	items, ok := response["rooms"].([]any)
	if !ok {
		t.Fatalf("rooms response missing rooms: %v", response)
	}
	for _, item := range items {
		room, ok := item.(map[string]any)
		if ok && room["id"] == roomID {
			return room
		}
	}
	t.Fatalf("room %s not found in response: %v", roomID, response)
	return nil
}

func roomMemberByID(t *testing.T, response map[string]any, userID string) map[string]any {
	t.Helper()
	items, ok := response["members"].([]any)
	if !ok {
		t.Fatalf("members response missing members: %v", response)
	}
	for _, item := range items {
		member, ok := item.(map[string]any)
		if !ok {
			continue
		}
		user, ok := member["user"].(map[string]any)
		if ok && user["id"] == userID {
			return member
		}
	}
	t.Fatalf("member %s not found in response: %v", userID, response)
	return nil
}

func listRoomMessages(t *testing.T, api *apiHarness, token, roomID string) []map[string]any {
	t.Helper()
	status, response := api.request(http.MethodGet, "/rooms/"+roomID+"/messages?limit=100", token, nil)
	api.requireStatus(status, http.StatusOK, response)
	items, ok := response["messages"].([]any)
	if !ok {
		t.Fatalf("messages response missing messages: %v", response)
	}
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("message item is not an object: %v", item)
		}
		messages = append(messages, msg)
	}
	return messages
}

func requireSystemMessage(t *testing.T, messages []map[string]any, event, subjectID string) map[string]any {
	t.Helper()
	for _, msg := range messages {
		if msg["type"] != systemMessageType {
			continue
		}
		attachment := systemAttachment(t, msg)
		if attachment["event"] != event {
			continue
		}
		if systemAttachmentSubjectID(t, attachment) == subjectID {
			return msg
		}
	}
	t.Fatalf("system message %s for %s not found: %v", event, subjectID, messages)
	return nil
}

func hasSystemMessage(t *testing.T, messages []map[string]any, event, subjectID string) bool {
	t.Helper()
	for _, msg := range messages {
		if msg["type"] != systemMessageType {
			continue
		}
		attachment := systemAttachment(t, msg)
		if attachment["event"] == event && systemAttachmentSubjectID(t, attachment) == subjectID {
			return true
		}
	}
	return false
}

func systemAttachment(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	items, ok := msg["attachments"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("system message missing attachments: %v", msg)
	}
	attachment, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("system attachment is not an object: %v", items[0])
	}
	return attachment
}

func systemAttachmentSubjectID(t *testing.T, attachment map[string]any) string {
	t.Helper()
	if target, ok := attachment["target"].(map[string]any); ok {
		return target["id"].(string)
	}
	user, ok := attachment["user"].(map[string]any)
	if !ok {
		t.Fatalf("system attachment missing user: %v", attachment)
	}
	return user["id"].(string)
}

func requireSystemRoleChange(t *testing.T, messages []map[string]any, subjectID, toRole string) map[string]any {
	t.Helper()
	for _, msg := range messages {
		if msg["type"] != systemMessageType {
			continue
		}
		attachment := systemAttachment(t, msg)
		if attachment["event"] == systemEventRoomRoleChanged &&
			systemAttachmentSubjectID(t, attachment) == subjectID &&
			attachment["to_role"] == toRole {
			return msg
		}
	}
	t.Fatalf("role-change system message for %s to %s not found: %v", subjectID, toRole, messages)
	return nil
}

func assertHistoryContainsExactly(t *testing.T, response map[string]any, messageID string) {
	t.Helper()
	messages, ok := response["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("expected exactly one history message: %v", response)
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["id"] != messageID {
		t.Fatalf("history message mismatch: %v", response)
	}
}

func assertHistoryContainsIDs(t *testing.T, response map[string]any, messageIDs ...string) {
	t.Helper()
	messages, ok := response["messages"].([]any)
	if !ok || len(messages) != len(messageIDs) {
		t.Fatalf("history message count mismatch: got %v want %v", response, messageIDs)
	}
	want := make(map[string]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		want[messageID] = struct{}{}
	}
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("invalid history message: %v", item)
		}
		messageID, _ := message["id"].(string)
		if _, ok := want[messageID]; !ok {
			t.Fatalf("unexpected history message: got %v want %v", response, messageIDs)
		}
		delete(want, messageID)
	}
	if len(want) != 0 {
		t.Fatalf("missing history messages: got %v want %v", response, messageIDs)
	}
}

func historyContains(response map[string]any, messageID string) bool {
	messages, _ := response["messages"].([]any)
	for _, item := range messages {
		message, _ := item.(map[string]any)
		if message["id"] == messageID {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
