package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
)

func names(assets ...string) []githubAsset {
	out := make([]githubAsset, len(assets))
	for i, name := range assets {
		out[i] = githubAsset{Name: name}
	}
	return out
}

func outputs(selected []selectedAsset) [][2]string {
	out := make([][2]string, len(selected))
	for i, item := range selected {
		out[i] = [2]string{item.Asset.Name, item.OutputName}
	}
	return out
}

func TestSelectReleaseAssets(t *testing.T) {
	cases := []struct {
		name   string
		assets []githubAsset
		want   [][2]string
	}{
		{
			"first of each installer, renamed",
			names("app.zip", "mac-a.dmg", "mac-b.dmg", "win-a.exe", "win-b.exe", "android-a.APK", "android-b.apk"),
			[][2]string{{"mac-a.dmg", "GangChat_v1.2.3.dmg"}, {"win-a.exe", "GangChat_v1.2.3.exe"}, {"android-a.APK", "GangChat_v1.2.3.apk"}},
		},
		{
			"installers, then blockmap, then feeds",
			names("appcast.xml", "GangChat-1.2.3-windows.zip", "GangChat_v1.2.3.apk", "GangChat_v1.2.3.dmg", "GangChat_v1.2.3.exe", "GangChat_v1.2.3.exe.blockmap", "latest.yml"),
			[][2]string{
				{"GangChat_v1.2.3.apk", "GangChat_v1.2.3.apk"},
				{"GangChat_v1.2.3.dmg", "GangChat_v1.2.3.dmg"},
				{"GangChat_v1.2.3.exe", "GangChat_v1.2.3.exe"},
				{"GangChat_v1.2.3.exe.blockmap", "GangChat_v1.2.3.exe.blockmap"},
				{"appcast.xml", "appcast.xml"},
				{"latest.yml", "latest.yml"},
			},
		},
		{
			"the blockmap follows its installer's name",
			names("Setup.exe", "Other.exe.blockmap", "setup.exe.blockmap"),
			[][2]string{{"Setup.exe", "GangChat_v1.2.3.exe"}, {"setup.exe.blockmap", "GangChat_v1.2.3.exe.blockmap"}},
		},
	}
	for _, tc := range cases {
		if got := outputs(selectReleaseAssets(tc.assets, "GangChat", "v1.2.3")); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReleaseFeedChecks(t *testing.T) {
	latestYml := func(name string) string {
		return "version: 1.2.3\nfiles:\n  - url: " + name + "\n    sha512: abc==\n    size: 101713257\npath: " + name + "\nsha512: abc==\nreleaseDate: '2026-10-16T19:21:39.078Z'\n"
	}
	if !latestYmlPointsAtFile(latestYml("GangChat_v1.2.3.exe"), "GangChat_v1.2.3.exe") ||
		!latestYmlPointsAtFile(latestYml("'GangChat_v1.2.3.exe'"), "GangChat_v1.2.3.exe") ||
		latestYmlPointsAtFile(latestYml("GangChat_v1.2.2.exe"), "GangChat_v1.2.3.exe") ||
		latestYmlPointsAtFile("version: 1.2.3\n", "GangChat_v1.2.3.exe") {
		t.Error("latest.yml check")
	}

	appcast := func(link string) string {
		return `<rss><channel><item><enclosure url="` + link + `" sparkle:edSignature="x" length="1"/></item></channel></rss>`
	}
	dmg := "https://ky-z.com/gang-chat/assets/releases/GangChat_v1.2.3.dmg"
	if !appcastPointsAt(appcast(dmg), dmg) ||
		appcastPointsAt(appcast("https://ky-z.com/gang-chat/assets/releases/GangChat_v1.2.2.dmg"), dmg) ||
		appcastPointsAt(appcast("https://data.ky-z.com:9000/gang-chat/releases/GangChat_v1.2.3.dmg"), dmg) ||
		appcastPointsAt("<rss><channel/></rss>", dmg) ||
		appcastPointsAt(appcast("not a url"), dmg) {
		t.Error("appcast check")
	}

	if !releaseKeyIsCleared("assets/releases/GangChat_v1.2.2.exe") || !releaseKeyIsCleared("assets/releases/latest.yml") ||
		releaseKeyIsCleared("assets/releases/GangChat_v1.2.2.exe.blockmap") {
		t.Error("earlier blockmaps must stay for differential updates")
	}
}

const testDownloadBase = "https://ky-z.com/gang-chat/assets/releases"

func TestReleaseSyncConfig(t *testing.T) {
	for _, repo := range []string{"gang-chat/gang-chat-client", "https://github.com/gang-chat/gang-chat-client", "git@github.com:gang-chat/gang-chat-client.git"} {
		sync, err := newReleaseSyncConfig(&config.Config{ReleaseSyncRepositoryURL: repo, ReleaseSyncAssetPrefix: "GangChat", ReleaseDownloadBaseURL: testDownloadBase + "/"})
		if err != nil || sync.Owner != "gang-chat" || sync.Repo != "gang-chat-client" ||
			sync.TargetPrefix != "assets/releases/" || sync.DownloadBase != testDownloadBase {
			t.Errorf("%s: %+v %v", repo, sync, err)
		}
	}
	if sync, err := newReleaseSyncConfig(&config.Config{}); sync != nil || err != nil {
		t.Error("no repository should mean no release sync")
	}
	for _, bad := range []*config.Config{
		{ReleaseSyncRepositoryURL: "https://gitlab.com/a/b", ReleaseSyncAssetPrefix: "GangChat", ReleaseDownloadBaseURL: testDownloadBase},
		{ReleaseSyncRepositoryURL: "a/b", ReleaseSyncAssetPrefix: "GangChat.exe", ReleaseDownloadBaseURL: testDownloadBase},
		{ReleaseSyncRepositoryURL: "a/b", ReleaseSyncAssetPrefix: "GangChat"},
		{ReleaseSyncRepositoryURL: "a/b", ReleaseSyncAssetPrefix: "GangChat", ReleaseDownloadBaseURL: "/gang-chat/assets/releases"},
	} {
		if _, err := newReleaseSyncConfig(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// fakeRepo is a GitHub repository with one release, tagged on commit "abc123",
// whose assets are the given name -> body pairs.
type fakeRepo struct {
	*httptest.Server
	mu sync.Mutex
	// runs is the commit's workflow runs, newest first.
	runs      []map[string]string
	downloads int
}

func (f *fakeRepo) setRuns(runs ...map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = runs
}

func releaseRun(status, conclusion string) map[string]string {
	return map[string]string{"path": ".github/workflows/release.yml", "status": status, "conclusion": conclusion}
}

func fakeGitHub(t *testing.T, tag string, files [][2]string) *fakeRepo {
	t.Helper()
	repo := &fakeRepo{}
	const root = "/repos/gang-chat/gang-chat-client"
	repo.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			for _, file := range files {
				if "/assets/"+file[0] == r.URL.Path {
					repo.downloads++
					w.Write([]byte(file[1]))
					return
				}
			}
			http.NotFound(w, r)
		case r.URL.Path == root+"/releases/tags/"+tag || r.URL.Path == root+"/releases/latest":
			release := githubRelease{ID: 1, TagName: tag, PublishedAt: "2026-10-21T20:05:32Z"}
			for i, file := range files {
				release.Assets = append(release.Assets, githubAsset{ID: int64(i + 1), Name: file[0], URL: repo.URL + "/assets/" + file[0], Size: int64(len(file[1]))})
			}
			json.NewEncoder(w).Encode(release)
		case r.URL.Path == root+"/commits/"+tag && r.Header.Get("Accept") == "application/vnd.github.sha":
			w.Write([]byte("abc123"))
		case r.URL.Path == root+"/actions/runs" && r.URL.Query().Get("head_sha") == "abc123":
			json.NewEncoder(w).Encode(map[string]any{"workflow_runs": repo.runs})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(repo.Close)
	return repo
}

func syncHarness(t *testing.T, github *fakeRepo) *harness {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.ReleaseSyncRepositoryURL = "https://github.com/gang-chat/gang-chat-client"
		cfg.ReleaseSyncAssetPrefix = "GangChat"
		cfg.ReleaseDownloadBaseURL = testDownloadBase
	})
	h.handler.GitHubAPI = github.URL
	for _, key := range []string{"assets/releases/GangChat_v0.1.0.exe", "assets/releases/GangChat_v0.1.0.exe.blockmap", "assets/releases/appcast.xml", "assets/ast_1/a.png"} {
		h.store.objects[key] = memoryObject{body: []byte("old")}
	}
	if err := releases.Save(h.db, releases.Release{Version: "0.1.0", Notes: "first", Published: true,
		Downloads: releases.Downloads{Windows: testDownloadBase + "/GangChat_v0.1.0.exe"}}); err != nil {
		t.Fatal(err)
	}
	return h
}

// releaseFiles is a release whose feeds offer its own installers.
var releaseFiles = [][2]string{
	{"Gang Chat.dmg", "dmg"},
	{"setup.exe", "exe"},
	{"setup.exe.blockmap", "map"},
	{"app.apk", "apk"},
	{"appcast.xml", `<rss><enclosure url="` + testDownloadBase + `/GangChat_v0.2.0.dmg"/></rss>`},
	{"latest.yml", "files:\n  - url: GangChat_v0.2.0.exe\npath: GangChat_v0.2.0.exe\n"},
}

func TestReleaseSyncReplacesTheStoredRelease(t *testing.T) {
	h := syncHarness(t, fakeGitHub(t, "v0.2.0", releaseFiles))

	status, out := h.admin(http.MethodPost, "/admin/release-sync", map[string]string{"tagName": "v0.2.0"}, nil)
	result := data(t, status, out)
	if result["deleted"].(float64) != 2 {
		t.Fatalf("expected the old installer and feed cleared: %v", result)
	}
	want := []string{
		"assets/ast_1/a.png",
		"assets/releases/GangChat_v0.1.0.exe.blockmap",
		"assets/releases/GangChat_v0.2.0.apk",
		"assets/releases/GangChat_v0.2.0.dmg",
		"assets/releases/GangChat_v0.2.0.exe",
		"assets/releases/GangChat_v0.2.0.exe.blockmap",
		"assets/releases/appcast.xml",
		"assets/releases/latest.yml",
	}
	if got := h.store.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("storage: %v", got)
	}
	puts := h.store.puts
	if puts[len(puts)-2] != "assets/releases/appcast.xml" || puts[len(puts)-1] != "assets/releases/latest.yml" {
		t.Fatalf("feeds must land last: %v", puts)
	}

	synced, err := releases.Get(h.db, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !synced.Published || synced.ReleaseDate != "2026-10-22" || synced.Title != "Gang Chat 0.2.0" ||
		synced.Downloads.MacOS != testDownloadBase+"/GangChat_v0.2.0.dmg" {
		t.Fatalf("synced release: %+v", synced)
	}
	old, _ := releases.Get(h.db, "0.1.0")
	if old.Downloads.Windows != "" || old.Notes != "first" {
		t.Fatalf("the old release's deleted installer must lose its link, nothing else: %+v", old)
	}
}

func TestReleaseSyncLeavesStorageAloneWhenAFeedPointsElsewhere(t *testing.T) {
	github := fakeGitHub(t, "v0.2.0", [][2]string{
		{"setup.exe", "exe"},
		{"latest.yml", "path: GangChat_v0.1.9.exe\n"},
	})
	h := syncHarness(t, github)
	before := h.store.keys()

	status, out := h.admin(http.MethodPost, "/admin/release-sync", map[string]string{"tagName": "v0.2.0"}, nil)
	if status != http.StatusBadRequest || errorCode(out) != "RELEASE_LATEST_YML_MISMATCH" {
		t.Fatalf("expected the mismatch refused: %d %v", status, out)
	}
	if got := h.store.keys(); !reflect.DeepEqual(got, before) {
		t.Fatalf("storage changed: %v", got)
	}
	var failed int
	h.db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 's3.release.sync' AND status = 'failed'`).Scan(&failed)
	if failed != 1 {
		t.Fatal("the refused sync must be audited")
	}
}

func TestReleaseSyncRunsOneAtATime(t *testing.T) {
	h := syncHarness(t, fakeGitHub(t, "v0.2.0", releaseFiles))
	h.handler.syncing.Lock()
	status, out := h.admin(http.MethodPost, "/admin/release-sync", map[string]string{"tagName": "v0.2.0"}, nil)
	h.handler.syncing.Unlock()
	if status != http.StatusConflict || errorCode(out) != "RELEASE_SYNC_RUNNING" {
		t.Fatalf("expected a second sync refused: %d %v", status, out)
	}
}

func TestReleaseWatchSyncsOnceTheReleaseWorkflowSucceeds(t *testing.T) {
	github := fakeGitHub(t, "v0.2.0", releaseFiles)
	h := syncHarness(t, github)
	check := func() string {
		t.Helper()
		tag, err := h.handler.checkLatestRelease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return tag
	}

	for _, runs := range [][]map[string]string{
		nil,
		{releaseRun("in_progress", "")},
		{releaseRun("completed", "failure")},
		// Only the newest release run counts, and only the release workflow.
		{releaseRun("completed", "failure"), releaseRun("completed", "success")},
		{{"path": ".github/workflows/android.yml", "status": "completed", "conclusion": "success"}},
	} {
		github.setRuns(runs...)
		if tag := check(); tag != "" || github.downloads != 0 {
			t.Fatalf("synced before the release workflow succeeded (%v): %q", runs, tag)
		}
	}

	github.setRuns(releaseRun("completed", "success"), releaseRun("completed", "failure"))
	if tag := check(); tag != "v0.2.0" {
		t.Fatalf("expected v0.2.0 synced, got %q", tag)
	}
	synced, err := releases.Get(h.db, "0.2.0")
	if err != nil || synced.Downloads.Windows != testDownloadBase+"/GangChat_v0.2.0.exe" || !synced.Published {
		t.Fatalf("synced release: %+v %v", synced, err)
	}
	var actor string
	h.db.QueryRow(`SELECT actor FROM admin_audit_log WHERE action = 's3.release.sync' AND target = 'v0.2.0' AND status = 'ok'`).Scan(&actor)
	if actor != "release-watch" {
		t.Fatalf("the watch's sync must be audited as its own: %q", actor)
	}

	// A synced release is left alone, even once the console deletes it.
	downloads := github.downloads
	if _, err := releases.Delete(h.db, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if tag := check(); tag != "" || github.downloads != downloads {
		t.Fatalf("synced v0.2.0 twice: %q", tag)
	}
}

func TestReleaseWatchWaitsAfterAFailedSync(t *testing.T) {
	github := fakeGitHub(t, "v0.2.0", [][2]string{
		{"setup.exe", "exe"},
		{"latest.yml", "path: GangChat_v0.1.9.exe\n"},
	})
	github.setRuns(releaseRun("completed", "success"))
	h := syncHarness(t, github)
	if _, err := h.handler.checkLatestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "latest.yml") {
		t.Fatalf("expected the mismatched feed refused: %v", err)
	}
	downloads := github.downloads
	if tag, err := h.handler.checkLatestRelease(context.Background()); tag != "" || err != nil || github.downloads != downloads {
		t.Fatalf("retried a failed sync right away: %q %v", tag, err)
	}
}
