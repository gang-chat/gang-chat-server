package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
)

// The release watch syncs each new client release on its own. GitHub shows a
// release as soon as the release workflow creates it, while the run is still
// uploading the files, so the watch waits until that run has succeeded; then
// it runs the same sync as the console, and the homepage, the downloads and
// the update feeds move to the new version.

const (
	releaseWatchInterval = 5 * time.Minute
	// releaseWatchRetryAfter is how long the watch leaves a tag alone after
	// failing to sync it.
	releaseWatchRetryAfter = time.Hour
	releaseWorkflowPath    = ".github/workflows/release.yml"
	releaseWatchActor      = "release-watch"
	releaseSyncAction      = "s3.release.sync"
)

// RunReleaseWatch checks the repository's latest release every five minutes
// until ctx ends. It does nothing when release sync is off.
func (h *Handler) RunReleaseWatch(ctx context.Context) {
	if h.Sync == nil {
		return
	}
	log.Printf("release watch: syncing new %s/%s releases once %s succeeds", h.Sync.Owner, h.Sync.Repo, releaseWorkflowPath)
	ticker := time.NewTicker(releaseWatchInterval)
	defer ticker.Stop()
	for {
		if tag, err := h.checkLatestRelease(ctx); err != nil {
			log.Printf("release watch: %v", err)
		} else if tag != "" {
			log.Printf("release watch: synced %s", tag)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkLatestRelease syncs the latest release unless it has been synced
// before, its release workflow hasn't succeeded yet, or another sync is
// running. It returns the tag it synced.
func (h *Handler) checkLatestRelease(ctx context.Context) (string, error) {
	var release githubRelease
	path := fmt.Sprintf("/repos/%s/%s/releases/latest", url.PathEscape(h.Sync.Owner), url.PathEscape(h.Sync.Repo))
	if err := h.githubJSON(ctx, path, &release); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	tag := strings.TrimSpace(release.TagName)
	if !releases.ValidVersion(strings.TrimPrefix(tag, "v")) {
		return "", nil
	}
	if done, err := h.syncedBefore(tag); err != nil || done {
		return "", err
	}
	if succeeded, err := h.releaseWorkflowSucceeded(ctx, tag); err != nil || !succeeded {
		return "", err
	}
	if !h.syncing.TryLock() {
		return "", nil
	}
	defer h.syncing.Unlock()
	syncCtx, cancel := context.WithTimeout(ctx, releaseSyncTimeout)
	defer cancel()
	result, err := h.runReleaseSync(syncCtx, tag)
	h.record(releaseWatchActor, releaseSyncAction, tag, result.auditDetail(), err)
	if err != nil {
		return "", fmt.Errorf("sync %s: %w", tag, err)
	}
	return tag, nil
}

// syncedBefore reports whether tag has been synced, by anyone, or the watch
// failed to sync it within releaseWatchRetryAfter. The audit log is the
// record: a version deleted or rolled back from the console stays that way.
func (h *Handler) syncedBefore(tag string) (bool, error) {
	var synced, lastFailure int64
	err := h.DB.QueryRow(
		`SELECT COALESCE(SUM(status = 'ok'), 0),
		        COALESCE(MAX(CASE WHEN status = 'failed' AND actor = ? THEN created_at END), 0)
		 FROM admin_audit_log WHERE action = ? AND target = ?`,
		releaseWatchActor, releaseSyncAction, tag,
	).Scan(&synced, &lastFailure)
	if err != nil {
		return false, err
	}
	return synced > 0 || time.Since(time.UnixMilli(lastFailure)) < releaseWatchRetryAfter, nil
}

// releaseWorkflowSucceeded reports whether the newest release workflow run on
// tag's commit has finished successfully. Re-running a failed job reruns that
// same run, so a release whose deploy failed syncs once the rerun succeeds.
func (h *Handler) releaseWorkflowSucceeded(ctx context.Context, tag string) (bool, error) {
	repo := fmt.Sprintf("/repos/%s/%s", url.PathEscape(h.Sync.Owner), url.PathEscape(h.Sync.Repo))
	resp, err := h.githubGet(ctx, repo+"/commits/"+url.PathEscape(tag), "application/vnd.github.sha")
	if err != nil {
		return false, err
	}
	sha, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if err != nil {
		return false, err
	}
	var runs struct {
		WorkflowRuns []struct {
			Path       string `json:"path"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	if err := h.githubJSON(ctx, repo+"/actions/runs?per_page=100&head_sha="+url.QueryEscape(strings.TrimSpace(string(sha))), &runs); err != nil {
		return false, err
	}
	// Runs come newest first.
	for _, run := range runs.WorkflowRuns {
		if run.Path == releaseWorkflowPath || strings.HasPrefix(run.Path, releaseWorkflowPath+"@") {
			return run.Status == "completed" && run.Conclusion == "success", nil
		}
	}
	return false, nil
}
