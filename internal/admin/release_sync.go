package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

// Release sync copies one GitHub release of the client into the storage
// directory's storage.ReleasePrefix: <asset prefix>_<tag>.dmg/.exe/.apk (plus
// the .exe's .blockmap) and the update feeds appcast.xml (Sparkle, macOS) and
// latest.yml (electron-updater, Windows), which the server then serves at
// /assets/releases. It clears the previous release's files first, then records
// the download links on the version's release so the homepage offers them.

const (
	sparkleAppcastName      = releases.SparkleAppcastName
	electronUpdaterFeedName = releases.ElectronUpdaterFeedName
	releaseSyncTimeout      = 30 * time.Minute
)

var releaseAssetExtensions = []string{".dmg", ".exe", ".apk"}

type releaseSyncConfig struct {
	RepositoryURL string
	Owner         string
	Repo          string
	TargetPrefix  string
	AssetPrefix   string
	Token         string
	// DownloadBase is release_download_base_url without its trailing slash.
	DownloadBase string
}

type releaseSyncSummary struct {
	Enabled       bool   `json:"enabled"`
	Repository    string `json:"repository,omitempty"`
	RepositoryURL string `json:"repositoryUrl,omitempty"`
	TargetPrefix  string `json:"targetPrefix,omitempty"`
	AssetPrefix   string `json:"assetPrefix,omitempty"`
}

func (s *releaseSyncConfig) summary() releaseSyncSummary {
	if s == nil {
		return releaseSyncSummary{}
	}
	return releaseSyncSummary{
		Enabled:       true,
		Repository:    s.Owner + "/" + s.Repo,
		RepositoryURL: s.RepositoryURL,
		TargetPrefix:  s.TargetPrefix,
		AssetPrefix:   s.AssetPrefix,
	}
}

var (
	repoShorthandPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)$`)
	repoSSHPattern       = regexp.MustCompile(`^git@github\.com:([^/]+)/(.+)$`)
)

// newReleaseSyncConfig reads the release_sync_* settings and
// release_download_base_url; it returns nil when no repository is set.
func newReleaseSyncConfig(cfg *config.Config) (*releaseSyncConfig, error) {
	repository := strings.TrimSpace(cfg.ReleaseSyncRepositoryURL)
	if repository == "" {
		return nil, nil
	}
	owner, repo, err := parseGitHubRepository(repository)
	if err != nil {
		return nil, err
	}
	asset := strings.TrimLeft(strings.TrimSpace(cfg.ReleaseSyncAssetPrefix), "/")
	if asset == "" || len(asset) > 255 || strings.ContainsAny(asset, "/.") || hasControlCharacter(asset) {
		return nil, fmt.Errorf("release_sync_asset_prefix must be a file name without an extension, such as GangChat")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.ReleaseDownloadBaseURL), "/")
	if parsed, err := url.Parse(base); err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, fmt.Errorf("release_download_base_url must be where /assets/releases is public, such as https://ky-z.com/gang-chat/assets/releases")
	}
	return &releaseSyncConfig{
		RepositoryURL: repository,
		Owner:         owner,
		Repo:          repo,
		TargetPrefix:  storage.ReleasePrefix,
		AssetPrefix:   asset,
		Token:         strings.TrimSpace(cfg.ReleaseSyncGitHubToken),
		DownloadBase:  base,
	}, nil
}

func parseGitHubRepository(input string) (string, string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(input), ".git")
	if m := repoShorthandPattern.FindStringSubmatch(trimmed); m != nil {
		return m[1], m[2], nil
	}
	if m := repoSSHPattern.FindStringSubmatch(trimmed); m != nil {
		return m[1], strings.TrimSuffix(m[2], ".git"), nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Hostname() != "github.com" {
		return "", "", fmt.Errorf("release_sync_repository_url must be a github.com repository")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("release_sync_repository_url must include the owner and repository")
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

type githubAsset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

type githubRelease struct {
	ID          int64         `json:"id"`
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt string        `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

type githubReleaseSummary struct {
	ID          int64  `json:"id"`
	TagName     string `json:"tagName"`
	Name        string `json:"name,omitempty"`
	HTMLURL     string `json:"htmlUrl,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
	Prerelease  bool   `json:"prerelease"`
	AssetCount  int    `json:"assetCount"`
}

type selectedAsset struct {
	Asset      githubAsset
	OutputName string
}

// selectReleaseAssets picks the first .dmg, .exe and .apk, renamed for
// storage, then the .exe's blockmap, then the update feeds. electron-updater
// fetches <installer>.blockmap next to the installer, so it follows the
// installer's new name; the feeds keep their fixed names and go last because
// they point at the installers.
func selectReleaseAssets(assets []githubAsset, assetPrefix, tagName string) []selectedAsset {
	var selected []selectedAsset
	seen := map[string]bool{}
	for _, asset := range assets {
		name := strings.ToLower(strings.TrimSpace(asset.Name))
		for _, extension := range releaseAssetExtensions {
			if strings.HasSuffix(name, extension) && !seen[extension] {
				seen[extension] = true
				selected = append(selected, selectedAsset{Asset: asset, OutputName: versionedAssetName(assetPrefix, tagName, extension)})
			}
		}
		if len(seen) == len(releaseAssetExtensions) {
			break
		}
	}
	for _, item := range selected {
		if !strings.HasSuffix(item.OutputName, ".exe") {
			continue
		}
		blockmap := strings.ToLower(strings.TrimSpace(item.Asset.Name)) + ".blockmap"
		for _, asset := range assets {
			if strings.ToLower(strings.TrimSpace(asset.Name)) == blockmap {
				selected = append(selected, selectedAsset{Asset: asset, OutputName: item.OutputName + ".blockmap"})
				break
			}
		}
		break
	}
	for _, feed := range []string{sparkleAppcastName, electronUpdaterFeedName} {
		for _, asset := range assets {
			if strings.ToLower(strings.TrimSpace(asset.Name)) == feed {
				selected = append(selected, selectedAsset{Asset: asset, OutputName: feed})
				break
			}
		}
	}
	return selected
}

var unsafeTagCharacters = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func versionedAssetName(assetPrefix, tagName, extension string) string {
	version := ""
	if tag := strings.TrimSpace(tagName); tag != "" {
		safe := strings.Trim(unsafeTagCharacters.ReplaceAllString(tag, "-"), "-")
		if safe == "" {
			safe = "release"
		}
		version = "_" + safe
	}
	return assetPrefix + version + extension
}

// releaseKeyIsCleared reports whether a sync deletes key before uploading.
// Blockmaps stay: electron-updater downloads the installed version's blockmap
// next to the new one to fetch only the changed blocks, so earlier releases'
// ones must outlive their installers. They are a few hundred KB each.
func releaseKeyIsCleared(key string) bool {
	return !strings.HasSuffix(strings.ToLower(key), ".blockmap")
}

var latestYmlFilePattern = regexp.MustCompile(`(?m)^\s*(?:-\s*)?(?:url|path):\s*(.+?)\s*$`)

// latestYmlPointsAtFile reports whether an electron-updater latest.yml only
// downloads fileName, which it resolves next to the feed.
func latestYmlPointsAtFile(latestYml, fileName string) bool {
	matches := latestYmlFilePattern.FindAllStringSubmatch(latestYml, -1)
	if len(matches) == 0 {
		return false
	}
	for _, match := range matches {
		name := match[1]
		if len(name) >= 2 && (name[0] == '"' || name[0] == '\'') && name[len(name)-1] == name[0] {
			name = name[1 : len(name)-1]
		}
		if name != fileName {
			return false
		}
	}
	return true
}

var appcastEnclosurePattern = regexp.MustCompile(`<enclosure\b[^>]*\burl="([^"]+)"`)

// appcastPointsAt reports whether every update in a Sparkle appcast downloads
// downloadURL.
func appcastPointsAt(appcastXML, downloadURL string) bool {
	matches := appcastEnclosurePattern.FindAllStringSubmatch(appcastXML, -1)
	if len(matches) == 0 {
		return false
	}
	for _, match := range matches {
		if strings.ReplaceAll(match[1], "&amp;", "&") != downloadURL {
			return false
		}
	}
	return true
}

func (h *Handler) requireSync() (*releaseSyncConfig, error) {
	if h.Sync == nil {
		return nil, fail(http.StatusNotFound, "RELEASE_SYNC_NOT_CONFIGURED",
			"Set release_sync_repository_url, release_sync_asset_prefix and release_download_base_url in config.json")
	}
	return h.Sync, nil
}

func (h *Handler) githubGet(ctx context.Context, pathOrURL, accept string) (*http.Response, error) {
	target := pathOrURL
	if !strings.HasPrefix(target, "http") {
		target = strings.TrimRight(h.GitHubAPI, "/") + pathOrURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "gang-chat-server")
	if h.Sync.Token != "" {
		// net/http drops this header when GitHub redirects an asset download
		// to its storage host.
		req.Header.Set("Authorization", "Bearer "+h.Sync.Token)
	}
	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "GITHUB_RELEASE_REQUEST_FAILED", "GitHub request failed: "+err.Error())
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		status := resp.StatusCode
		if status >= 500 {
			status = http.StatusBadGateway
		}
		return nil, fail(status, "GITHUB_RELEASE_REQUEST_FAILED", "GitHub request failed: "+resp.Status)
	}
	return resp, nil
}

func (h *Handler) githubJSON(ctx context.Context, path string, dest any) error {
	resp, err := h.githubGet(ctx, path, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fail(http.StatusBadGateway, "GITHUB_RELEASE_REQUEST_FAILED", "GitHub sent an unreadable response")
	}
	return nil
}

func (h *Handler) listGitHubReleases(c *gin.Context) (any, error) {
	sync, err := h.requireSync()
	if err != nil {
		return nil, err
	}
	var list []githubRelease
	path := fmt.Sprintf("/repos/%s/%s/releases?per_page=100", url.PathEscape(sync.Owner), url.PathEscape(sync.Repo))
	if err := h.githubJSON(c.Request.Context(), path, &list); err != nil {
		return nil, err
	}
	out := []githubReleaseSummary{}
	for _, release := range list {
		if release.TagName == "" || release.Draft {
			continue
		}
		out = append(out, githubReleaseSummary{
			ID:          release.ID,
			TagName:     release.TagName,
			Name:        release.Name,
			HTMLURL:     release.HTMLURL,
			PublishedAt: release.PublishedAt,
			Prerelease:  release.Prerelease,
			AssetCount:  len(selectReleaseAssets(release.Assets, "x", "")),
		})
	}
	return out, nil
}

type syncedObject struct {
	Name        string `json:"name"`
	SourceName  string `json:"sourceName"`
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType,omitempty"`
}

type syncResult struct {
	Repository   string            `json:"repository"`
	TagName      string            `json:"tagName"`
	TargetPrefix string            `json:"targetPrefix"`
	Deleted      int               `json:"deleted"`
	Uploaded     []syncedObject    `json:"uploaded"`
	Release      *releases.Release `json:"release,omitempty"`
}

// downloadedAsset is a GitHub asset saved to a temporary file, so nothing in
// storage changes until every download has succeeded.
type downloadedAsset struct {
	selectedAsset
	File *os.File
	Size int64
}

func (h *Handler) syncRelease(c *gin.Context) (any, error) {
	var req struct {
		TagName string `json:"tagName"`
	}
	if err := bindJSON(c, &req); err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(req.TagName)
	if !h.syncing.TryLock() {
		return nil, fail(http.StatusConflict, "RELEASE_SYNC_RUNNING", "Another release sync is running; try again when it finishes")
	}
	defer h.syncing.Unlock()
	// The sync keeps going if the browser goes away: stopping between clearing
	// the old release and uploading the new one would leave no release.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), releaseSyncTimeout)
	defer cancel()
	result, err := h.runReleaseSync(ctx, tag)
	h.audit(c, releaseSyncAction, tag, result.auditDetail(), err)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *syncResult) auditDetail() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%s@%s: uploaded %d, deleted %d", r.Repository, r.TagName, len(r.Uploaded), r.Deleted)
}

// runReleaseSync copies tag's release into storage. Callers hold h.syncing.
func (h *Handler) runReleaseSync(ctx context.Context, tag string) (*syncResult, error) {
	sync, err := h.requireSync()
	if err != nil {
		return nil, err
	}
	store, err := h.requireObjects()
	if err != nil {
		return nil, err
	}
	if tag == "" || len(tag) > 255 {
		return nil, fail(http.StatusBadRequest, "INVALID_RELEASE_TAG", "Release tag is required")
	}
	var release githubRelease
	path := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", url.PathEscape(sync.Owner), url.PathEscape(sync.Repo), url.PathEscape(tag))
	if err := h.githubJSON(ctx, path, &release); err != nil {
		return nil, err
	}

	selected := selectReleaseAssets(release.Assets, sync.AssetPrefix, release.TagName)
	installerKeys := map[string]string{}
	for _, item := range selected {
		if !releases.IsUpdateFeed(item.OutputName) && !strings.HasSuffix(item.OutputName, ".blockmap") {
			installerKeys[item.OutputName[strings.LastIndex(item.OutputName, "."):]] = sync.TargetPrefix + item.OutputName
		}
	}
	if len(installerKeys) == 0 {
		return nil, fail(http.StatusBadRequest, "GITHUB_RELEASE_HAS_NO_SUPPORTED_ASSETS", "Selected release has no .dmg, .exe, or .apk assets")
	}

	dir, err := os.MkdirTemp("", "gang-release-sync-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	downloads := make([]downloadedAsset, 0, len(selected))
	defer func() {
		for _, d := range downloads {
			d.File.Close()
		}
	}()
	for _, item := range selected {
		d, err := h.downloadAsset(ctx, dir, item)
		if err != nil {
			return nil, err
		}
		downloads = append(downloads, d)
	}

	// Check the feeds before touching storage, so a release whose feed offers
	// some other file leaves the current one in place.
	for _, d := range downloads {
		if !releases.IsUpdateFeed(d.OutputName) {
			continue
		}
		body, err := readAll(d.File)
		if err != nil {
			return nil, err
		}
		if d.OutputName == sparkleAppcastName {
			if key := installerKeys[".dmg"]; key == "" || !appcastPointsAt(body, h.downloadURL(key)) {
				return nil, fail(http.StatusBadRequest, "RELEASE_APPCAST_MISMATCH",
					"appcast.xml must download the release's .dmg from release_download_base_url")
			}
		} else {
			exe := installerKeys[".exe"]
			if exe == "" || !latestYmlPointsAtFile(body, exe[len(sync.TargetPrefix):]) {
				return nil, fail(http.StatusBadRequest, "RELEASE_LATEST_YML_MISMATCH", "latest.yml must download the .exe from the same release")
			}
		}
	}

	deleted, err := clearReleasePrefix(ctx, store, sync.TargetPrefix)
	if err != nil {
		return nil, err
	}
	result := &syncResult{
		Repository:   sync.Owner + "/" + sync.Repo,
		TagName:      release.TagName,
		TargetPrefix: sync.TargetPrefix,
		Deleted:      deleted,
		Uploaded:     []syncedObject{},
	}
	// downloads is already in upload order: installers, blockmap, then feeds.
	for _, d := range downloads {
		key := sync.TargetPrefix + d.OutputName
		if _, err := d.File.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if err := store.Put(ctx, key, d.File); err != nil {
			return nil, err
		}
		result.Uploaded = append(result.Uploaded, syncedObject{Name: d.OutputName, SourceName: d.Asset.Name, Key: key, Size: d.Size,
			ContentType: releases.FileContentType(d.OutputName)})
	}

	saved, err := h.recordSyncedRelease(release, installerKeys)
	if err != nil {
		return nil, err
	}
	result.Release = saved
	return result, nil
}

func (h *Handler) downloadAsset(ctx context.Context, dir string, item selectedAsset) (downloadedAsset, error) {
	resp, err := h.githubGet(ctx, item.Asset.URL, "application/octet-stream")
	if err != nil {
		return downloadedAsset{}, err
	}
	defer resp.Body.Close()
	file, err := os.CreateTemp(dir, "asset-")
	if err != nil {
		return downloadedAsset{}, err
	}
	size, err := io.Copy(file, resp.Body)
	if err != nil {
		file.Close()
		return downloadedAsset{}, fail(http.StatusBadGateway, "GITHUB_ASSET_DOWNLOAD_FAILED", "Downloading "+item.Asset.Name+" failed")
	}
	if size == 0 {
		file.Close()
		return downloadedAsset{}, fail(http.StatusBadGateway, "GITHUB_ASSET_DOWNLOAD_FAILED", "GitHub asset is empty: "+item.Asset.Name)
	}
	return downloadedAsset{selectedAsset: item, File: file, Size: size}, nil
}

func readAll(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(file, 8<<20))
	return string(body), err
}

func clearReleasePrefix(ctx context.Context, store objectStore, prefix string) (int, error) {
	if strings.TrimSpace(prefix) == "" {
		return 0, fail(http.StatusBadRequest, "INVALID_RELEASE_PREFIX", "Release sync prefix cannot be empty")
	}
	var keys []string
	token := ""
	for {
		page, err := store.List(ctx, prefix, "", token, 1000)
		if err != nil {
			return 0, err
		}
		for _, object := range page.Objects {
			if releaseKeyIsCleared(object.Key) {
				keys = append(keys, object.Key)
			}
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	for _, key := range keys {
		if err := store.Delete(ctx, key); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// downloadURL is where the server serves the release file at key, which is
// under the sync's TargetPrefix.
func (h *Handler) downloadURL(key string) string {
	return h.Sync.DownloadBase + "/" + url.PathEscape(strings.TrimPrefix(key, h.Sync.TargetPrefix))
}

// releaseDateZone dates releases the way the homepage's announcements are
// dated.
var releaseDateZone = time.FixedZone("UTC+8", 8*60*60)

// recordSyncedRelease points the version's release at the files just uploaded,
// creating it (published, so the homepage moves on) if it is new, and clears
// the links of other versions, whose files the sync deleted.
func (h *Handler) recordSyncedRelease(release githubRelease, installerKeys map[string]string) (*releases.Release, error) {
	version := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	if !releases.ValidVersion(version) {
		return nil, nil
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	saved, err := releases.Get(tx, version)
	if err == releases.ErrNotFound {
		saved = &releases.Release{Version: version, Title: "Gang Chat " + version, Published: true}
		if published, err := time.Parse(time.RFC3339, release.PublishedAt); err == nil {
			saved.ReleaseDate = published.In(releaseDateZone).Format("2006-01-02")
		}
	} else if err != nil {
		return nil, err
	}
	saved.Downloads = releases.Downloads{}
	if key := installerKeys[".exe"]; key != "" {
		saved.Downloads.Windows = h.downloadURL(key)
	}
	if key := installerKeys[".dmg"]; key != "" {
		saved.Downloads.MacOS = h.downloadURL(key)
	}
	if key := installerKeys[".apk"]; key != "" {
		saved.Downloads.Android = h.downloadURL(key)
	}
	stale := h.Sync.DownloadBase + "/"
	for _, column := range []string{"windows_url", "macos_url", "android_url"} {
		if _, err := tx.Exec(
			`UPDATE app_releases SET `+column+` = '' WHERE version <> ? AND substr(`+column+`, 1, length(?)) = ?`,
			version, stale, stale,
		); err != nil {
			return nil, err
		}
	}
	if err := releases.Save(tx, *saved); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return releases.Get(h.DB, version)
}
