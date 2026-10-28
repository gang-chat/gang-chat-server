package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	gangdb "github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
)

const (
	testAdminUser     = "ops"
	testAdminPassword = "Correct-Horse-9-Battery"
)

type memoryObject struct {
	body []byte
}

// memoryStore is an in-memory storage directory.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string]memoryObject
	puts    []string
}

func newMemoryStore() *memoryStore { return &memoryStore{objects: map[string]memoryObject{}} }

func (m *memoryStore) List(_ context.Context, prefix, delimiter, _ string, _ int32) (objectList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := objectList{Prefix: prefix, Prefixes: []string{}, Objects: []objectSummary{}}
	seen := map[string]bool{}
	var keys []string
	for key := range m.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := key[len(prefix):]
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				p := prefix + rest[:i+1]
				if !seen[p] {
					seen[p] = true
					list.Prefixes = append(list.Prefixes, p)
				}
				continue
			}
		}
		list.Objects = append(list.Objects, objectSummary{Key: key, Size: int64(len(m.objects[key].body))})
	}
	return list, nil
}

func (m *memoryStore) Head(_ context.Context, key string) (objectMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok {
		return objectMetadata{}, errObjectNotFound
	}
	return objectMetadata{objectSummary: objectSummary{Key: key, Size: int64(len(object.body))}, ContentType: contentTypeOf(key), Metadata: map[string]string{}}, nil
}

func (m *memoryStore) Get(_ context.Context, key string) (io.ReadCloser, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok {
		return nil, "", errObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(object.body)), contentTypeOf(key), nil
}

func (m *memoryStore) Put(_ context.Context, key string, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memoryObject{body: data}
	m.puts = append(m.puts, key)
	return nil
}

func (m *memoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memoryStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for key := range m.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type harness struct {
	t       *testing.T
	router  *gin.Engine
	db      *sql.DB
	handler *Handler
	store   *memoryStore
	token   string
}

func newHarness(t *testing.T, configure func(*config.Config)) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "gang-chat.db")
	pool, err := gangdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	cfg := &config.Config{
		DatabasePath:           path,
		JWTSecret:              "test-secret",
		AccessTokenTTLSeconds:  900,
		RefreshTokenTTLSeconds: 3600,
		LoginMaxAttempts:       5,
		LoginWindowSeconds:     900,
		AdminUsername:          testAdminUser,
		AdminPassword:          testAdminPassword,
	}
	if configure != nil {
		configure(cfg)
	}
	router := gin.New()
	api := router.Group("/api/v1")
	auth.RegisterRoutes(api, pool, cfg, eventbus.New())
	handler, err := RegisterRoutes(api, pool, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	releases.RegisterPublicRoutes(api, pool)
	store := newMemoryStore()
	handler.Objects = store
	return &harness{t: t, router: router, db: pool, handler: handler, store: store}
}

func (h *harness) do(method, path, token string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// admin sends a request with the console's token, signing in first.
func (h *harness) admin(method, path string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	if h.token == "" {
		status, out := h.do(http.MethodPost, "/admin/login", "", map[string]string{"username": testAdminUser, "password": testAdminPassword}, nil)
		if status != http.StatusOK {
			h.t.Fatalf("admin login: %d %v", status, out)
		}
		h.token = out["data"].(map[string]any)["token"].(string)
	}
	return h.do(method, path, h.token, body, headers)
}

func errorCode(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func data(t *testing.T, status int, out map[string]any) map[string]any {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	value, ok := out["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not an object: %v", out)
	}
	return value
}

func (h *harness) insertUser(username, password string) string {
	h.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		h.t.Fatal(err)
	}
	uid, err := idgen.NextUserUID(h.db)
	if err != nil {
		h.t.Fatal(err)
	}
	id := "user-" + username
	if err := model.InsertUser(h.db, id, uid, username, model.Normalize(username), username+"@example.com", username+"@example.com", hash); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) insertRoom(id, name string) {
	h.t.Helper()
	now := time.Now().UnixMilli()
	if _, err := h.db.Exec(
		`INSERT INTO rooms (id, rid, name, default_avatar_key, description, created_at, updated_at)
		 VALUES (?, ?, ?, 'blue-3', '', ?, ?)`, id, "9"+id, name, now, now); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) appLogin(username, password string) int {
	status, _ := h.do(http.MethodPost, "/auth/login", "", map[string]string{"login": username, "password": password}, nil)
	return status
}

func TestAdminSignIn(t *testing.T) {
	h := newHarness(t, nil)

	status, out := h.do(http.MethodGet, "/admin/overview", "", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("overview without a token: %d %v", status, out)
	}
	status, out = h.do(http.MethodPost, "/admin/login", "", map[string]string{"username": testAdminUser, "password": "wrong"}, nil)
	if status != http.StatusUnauthorized || errorCode(out) != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password: %d %v", status, out)
	}
	status, out = h.admin(http.MethodGet, "/admin/me", nil, nil)
	if me := data(t, status, out); me["username"] != testAdminUser {
		t.Fatalf("me: %v", me)
	}
	status, out = h.admin(http.MethodPost, "/admin/logout", nil, nil)
	data(t, status, out)
	status, _ = h.do(http.MethodGet, "/admin/me", h.token, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("a signed-out token still works: %d", status)
	}

	var audited int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'auth.login'`).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("expected both sign-in attempts audited, got %d (%v)", audited, err)
	}
}

func TestAdminLocksOutAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < maxFailedLogins; i++ {
		h.do(http.MethodPost, "/admin/login", "", map[string]string{"username": testAdminUser, "password": "wrong"}, nil)
	}
	status, out := h.do(http.MethodPost, "/admin/login", "", map[string]string{"username": testAdminUser, "password": testAdminPassword}, nil)
	if status != http.StatusTooManyRequests || errorCode(out) != "LOGIN_LOCKED" {
		t.Fatalf("expected a lockout, got %d %v", status, out)
	}
}

func TestAdminStaysOffWithAWeakPassword(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.AdminPassword = "ops12" })
	status, out := h.do(http.MethodPost, "/admin/login", "", map[string]string{"username": testAdminUser, "password": "ops12"}, nil)
	if status != http.StatusServiceUnavailable || errorCode(out) != "ADMIN_DISABLED" {
		t.Fatalf("expected the console off, got %d %v", status, out)
	}
	if message := out["error"].(map[string]any)["message"].(string); strings.Contains(message, "characters") {
		t.Fatalf("the password's weaknesses must stay in the log: %q", message)
	}
	status, out = h.do(http.MethodGet, "/admin/status", "", nil, nil)
	if enabled := data(t, status, out)["enabled"]; enabled != false {
		t.Fatalf("status should report the console off: %v", out)
	}
}

func TestPasswordPolicy(t *testing.T) {
	if issues := passwordPolicyIssues(testAdminPassword, testAdminUser); len(issues) != 0 {
		t.Fatalf("a strong password was refused: %v", issues)
	}
	for _, weak := range []string{"short-A1", "alllowercase-123", "ALLUPPERCASE-123", "NoDigitsHere-Ok", "NoSymbols1234Ab", "Ops-is-in-here-1A", "Password-1234-Ab"} {
		if len(passwordPolicyIssues(weak, testAdminUser)) == 0 {
			t.Errorf("%q passed the policy", weak)
		}
	}
}

func TestAdminAccountLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	h.insertUser("taken", "taken-password")
	h.insertRoom("room-1", "Lobby")

	status, out := h.admin(http.MethodPost, "/admin/accounts", map[string]string{"username": "x", "email": "bad", "password": "short"}, nil)
	if status != http.StatusBadRequest || errorCode(out) != "VALIDATION_ERROR" {
		t.Fatalf("invalid input: %d %v", status, out)
	}
	status, out = h.admin(http.MethodPost, "/admin/accounts", map[string]string{
		"username": "Newbie", "email": "newbie@example.com", "password": "first-password", "displayName": "New Person",
	}, nil)
	created := data(t, status, out)
	id := created["id"].(string)
	if created["displayName"] != "New Person" || created["status"] != "active" || created["uid"] == "" {
		t.Fatalf("created account: %v", created)
	}
	if h.appLogin("newbie", "first-password") != http.StatusOK {
		t.Fatal("the new account can't sign in to the app")
	}

	status, out = h.admin(http.MethodGet, "/admin/accounts?q=NEWB", nil, nil)
	if list := data(t, status, out); list["total"].(float64) != 1 {
		t.Fatalf("search: %v", list)
	}
	status, out = h.admin(http.MethodGet, "/admin/accounts?q=new_", nil, nil)
	if list := data(t, status, out); list["total"].(float64) != 0 {
		t.Fatalf("_ must match itself, not any character: %v", list)
	}

	status, out = h.admin(http.MethodPatch, "/admin/accounts/"+id, map[string]string{"username": "TAKEN"}, nil)
	if status != http.StatusConflict || errorCode(out) != "ACCOUNT_CONFLICT" {
		t.Fatalf("duplicate username: %d %v", status, out)
	}
	status, out = h.admin(http.MethodPatch, "/admin/accounts/"+id, map[string]string{"email": "moved@example.com"}, nil)
	if updated := data(t, status, out); updated["email"] != "moved@example.com" || updated["emailVerified"] != false {
		t.Fatalf("a changed email must need verifying again: %v", updated)
	}
	status, out = h.admin(http.MethodPatch, "/admin/accounts/"+id, map[string]string{"status": "suspended"}, nil)
	if updated := data(t, status, out); updated["status"] != "suspended" || updated["activeSessionCount"].(float64) != 0 {
		t.Fatalf("suspending must sign the account out: %v", updated)
	}
	h.admin(http.MethodPatch, "/admin/accounts/"+id, map[string]string{"status": "active"}, nil)

	status, out = h.admin(http.MethodPost, "/admin/accounts/"+id+"/password", map[string]string{"password": "second-password"}, nil)
	data(t, status, out)
	if h.appLogin("newbie", "first-password") == http.StatusOK || h.appLogin("newbie", "second-password") != http.StatusOK {
		t.Fatal("the password reset didn't take")
	}

	status, out = h.admin(http.MethodPost, "/admin/accounts/"+id+"/memberships", map[string]string{"roomId": "room-1", "role": "member"}, nil)
	if added := data(t, status, out); added["role"] != "owner" {
		t.Fatalf("the first member of an ownerless room becomes its owner: %v", added)
	}
	status, out = h.admin(http.MethodDelete, "/admin/accounts/"+id+"/memberships/room-1", nil, nil)
	if status != http.StatusConflict || errorCode(out) != "OWNER_ROLE_LOCKED" {
		t.Fatalf("removing the owner: %d %v", status, out)
	}
	status, out = h.admin(http.MethodGet, "/admin/accounts/"+id, nil, nil)
	if detail := data(t, status, out); len(detail["memberships"].([]any)) != 1 {
		t.Fatalf("memberships: %v", detail)
	}

	status, out = h.admin(http.MethodDelete, "/admin/accounts/"+id, nil, map[string]string{"x-ops-confirmation": "wrong"})
	if status != http.StatusBadRequest || errorCode(out) != "DESTRUCTIVE_CONFIRMATION_REQUIRED" {
		t.Fatalf("delete without confirmation: %d %v", status, out)
	}
	status, out = h.admin(http.MethodDelete, "/admin/accounts/"+id, nil, map[string]string{"x-ops-confirmation": "Newbie"})
	data(t, status, out)
	status, _ = h.admin(http.MethodGet, "/admin/accounts/"+id, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("deleted account still found: %d", status)
	}
}

func TestAdminLeavesTheSuperUserAlone(t *testing.T) {
	h := newHarness(t, nil)
	var id string
	if err := h.db.QueryRow(`SELECT id FROM users WHERE is_superuser = 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	status, out := h.admin(http.MethodPost, "/admin/accounts/"+id+"/password", map[string]string{"password": "new-password"}, nil)
	if status != http.StatusForbidden || errorCode(out) != "SUPERUSER_READ_ONLY" {
		t.Fatalf("super user password reset: %d %v", status, out)
	}
}

func TestAdminBrowsesTheDatabase(t *testing.T) {
	h := newHarness(t, nil)
	h.insertUser("reader", "reader-password")

	status, out := h.admin(http.MethodGet, "/admin/db/tables", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("tables: %d %v", status, out)
	}
	found := false
	for _, table := range out["data"].([]any) {
		if row := table.(map[string]any); row["name"] == "users" {
			found = row["rows"].(float64) >= 1
		}
	}
	if !found {
		t.Fatalf("users table missing or empty: %v", out)
	}

	status, out = h.admin(http.MethodGet, "/admin/db/tables/users/schema", nil, nil)
	if status != http.StatusOK || out["data"].([]any)[0].(map[string]any)["key"] != "PRI" {
		t.Fatalf("users schema: %d %v", status, out)
	}
	status, out = h.admin(http.MethodGet, "/admin/db/tables/users/rows?limit=1", nil, nil)
	if status != http.StatusOK || len(out["data"].([]any)) != 1 {
		t.Fatalf("rows: %d %v", status, out)
	}
	status, _ = h.admin(http.MethodGet, `/admin/db/tables/users%22%20--/rows`, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("a name that isn't a table must not reach the SQL: %d", status)
	}
}

func TestAdminReleasesFeedTheHomepage(t *testing.T) {
	h := newHarness(t, nil)

	status, out := h.admin(http.MethodPost, "/admin/releases", map[string]any{"version": "1.2"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad version: %d %v", status, out)
	}
	for _, release := range []map[string]any{
		{"version": "0.9.0", "notes": "older", "release_date": "2026-07-01", "published": true},
		{"version": "v0.10.0", "notes": "newer", "release_date": "2026-08-01", "published": true,
			"downloads": map[string]string{"windows": "https://dl.test/a.exe"}},
		{"version": "0.11.0", "notes": "draft", "published": false},
	} {
		status, out := h.admin(http.MethodPost, "/admin/releases", release, nil)
		data(t, status, out)
	}
	status, out = h.admin(http.MethodPost, "/admin/releases", map[string]any{"version": "0.9.0"}, nil)
	if status != http.StatusConflict {
		t.Fatalf("duplicate version: %d %v", status, out)
	}
	status, out = h.admin(http.MethodPatch, "/admin/releases/0.10.0", map[string]any{"release_date": "2026/08/01"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad date: %d %v", status, out)
	}
	status, out = h.admin(http.MethodPatch, "/admin/releases/0.10.0", map[string]any{"notes": "edited", "minimum_supported_version": "0.9.0"}, nil)
	if updated := data(t, status, out); updated["notes"] != "edited" || updated["title"] != "Gang Chat 0.10.0" ||
		updated["downloads"].(map[string]any)["windows"] != "https://dl.test/a.exe" {
		t.Fatalf("update: %v", updated)
	}

	status, out = h.do(http.MethodGet, "/releases", "", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("public releases: %d %v", status, out)
	}
	list := out["releases"].([]any)
	if len(list) != 2 || out["latest"].(map[string]any)["version"] != "0.10.0" || list[1].(map[string]any)["version"] != "0.9.0" {
		t.Fatalf("homepage must see published releases, newest first: %v", out)
	}

	status, _ = h.admin(http.MethodDelete, "/admin/releases/0.9.0", nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("delete without confirmation: %d", status)
	}
	status, out = h.admin(http.MethodDelete, "/admin/releases/0.9.0", nil, map[string]string{"x-ops-confirmation": "0.9.0"})
	data(t, status, out)
}

func TestAdminUploadsAndDeletesObjects(t *testing.T) {
	h := newHarness(t, nil)
	send := func(key string, fields map[string]string) (int, map[string]any) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("file", "notes.txt")
		part.Write([]byte("hello"))
		form.WriteField("key", key)
		for name, value := range fields {
			form.WriteField(name, value)
		}
		form.Close()
		h.admin(http.MethodGet, "/admin/me", nil, nil) // signs in
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/storage/objects", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	status, out := send("docs/notes.txt", nil)
	if uploaded := data(t, status, out); uploaded["key"] != "docs/notes.txt" {
		t.Fatalf("upload: %v", uploaded)
	}
	status, out = send("docs/notes.txt", nil)
	if status != http.StatusConflict {
		t.Fatalf("overwrite without asking: %d %v", status, out)
	}
	for _, key := range []string{"../escape", "docs/.hidden", "/etc/passwd"} {
		status, out = send(key, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("unsafe key %q: %d %v", key, status, out)
		}
	}
	status, out = h.admin(http.MethodGet, "/admin/storage/objects?prefix=docs/", nil, nil)
	if list := data(t, status, out); len(list["objects"].([]any)) != 1 {
		t.Fatalf("list: %v", list)
	}
	status, out = h.admin(http.MethodDelete, "/admin/storage/objects", map[string]string{"key": "docs/notes.txt", "confirmation": "docs/notes.txt"}, nil)
	data(t, status, out)
	if keys := h.store.keys(); len(keys) != 0 {
		t.Fatalf("object not deleted: %v", keys)
	}
}
