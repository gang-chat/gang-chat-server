package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
)

// Releases use the same snake_case shape here as on the public endpoint.

type downloadsInput struct {
	Windows *string `json:"windows"`
	MacOS   *string `json:"macos"`
	Android *string `json:"android"`
}

type releaseInput struct {
	Version                 *string         `json:"version"`
	Title                   *string         `json:"title"`
	Notes                   *string         `json:"notes"`
	ReleaseDate             *string         `json:"release_date"`
	Published               *bool           `json:"published"`
	MinimumSupportedVersion *string         `json:"minimum_supported_version"`
	Downloads               *downloadsInput `json:"downloads"`
}

func (h *Handler) listReleases(*gin.Context) (any, error) {
	return releases.List(h.DB, false)
}

// apply copies the fields that are set onto r and checks the result.
func (in releaseInput) apply(r *releases.Release) error {
	set := func(dest *string, value *string) {
		if value != nil {
			*dest = strings.TrimSpace(*value)
		}
	}
	set(&r.Title, in.Title)
	set(&r.Notes, in.Notes)
	set(&r.ReleaseDate, in.ReleaseDate)
	set(&r.MinimumSupportedVersion, in.MinimumSupportedVersion)
	if in.Published != nil {
		r.Published = *in.Published
	}
	if in.Downloads != nil {
		set(&r.Downloads.Windows, in.Downloads.Windows)
		set(&r.Downloads.MacOS, in.Downloads.MacOS)
		set(&r.Downloads.Android, in.Downloads.Android)
	}
	if r.Title == "" {
		r.Title = "Gang Chat " + r.Version
	}
	if utf8.RuneCountInString(r.Title) > 120 {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Title must be at most 120 characters")
	}
	if utf8.RuneCountInString(r.Notes) > 4000 {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Notes must be at most 4000 characters")
	}
	if !releases.ValidDate(r.ReleaseDate) {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Release date must be YYYY-MM-DD")
	}
	if m := r.MinimumSupportedVersion; m != "" && (!releases.ValidVersion(m) || releases.CompareVersions(m, r.Version) > 0) {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Minimum supported version must be x.y.z and not newer than the release")
	}
	for _, link := range []string{r.Downloads.Windows, r.Downloads.MacOS, r.Downloads.Android} {
		if link == "" {
			continue
		}
		parsed, err := url.Parse(link)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || len(link) > 2048 {
			return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Download links must be http(s) URLs")
		}
	}
	return nil
}

func (h *Handler) createRelease(c *gin.Context) (any, error) {
	var in releaseInput
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	version := ""
	if in.Version != nil {
		version = strings.TrimPrefix(strings.TrimSpace(*in.Version), "v")
	}
	err := func() error {
		if !releases.ValidVersion(version) {
			return fail(http.StatusBadRequest, "VALIDATION_ERROR", "Version must be x.y.z")
		}
		r := releases.Release{Version: version}
		if err := in.apply(&r); err != nil {
			return err
		}
		tx, err := h.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := releases.Get(tx, version); err == nil {
			return fail(http.StatusConflict, "RELEASE_EXISTS", "Version "+version+" already exists")
		} else if !errors.Is(err, releases.ErrNotFound) {
			return err
		}
		if err := releases.Save(tx, r); err != nil {
			return err
		}
		return tx.Commit()
	}()
	h.audit(c, "releases.create", version, "", err)
	if err != nil {
		return nil, err
	}
	return releases.Get(h.DB, version)
}

func (h *Handler) updateRelease(c *gin.Context) (any, error) {
	version := c.Param("version")
	var in releaseInput
	if err := bindJSON(c, &in); err != nil {
		return nil, err
	}
	err := func() error {
		r, err := releases.Get(h.DB, version)
		if errors.Is(err, releases.ErrNotFound) {
			return fail(http.StatusNotFound, "RELEASE_NOT_FOUND", "Release not found")
		}
		if err != nil {
			return err
		}
		if err := in.apply(r); err != nil {
			return err
		}
		return releases.Save(h.DB, *r)
	}()
	h.audit(c, "releases.update", version, "", err)
	if err != nil {
		return nil, err
	}
	return releases.Get(h.DB, version)
}

func (h *Handler) deleteRelease(c *gin.Context) (any, error) {
	version := c.Param("version")
	err := func() error {
		if c.GetHeader("x-ops-confirmation") != version {
			return fail(http.StatusBadRequest, "DESTRUCTIVE_CONFIRMATION_REQUIRED", "Type the exact version to delete it")
		}
		deleted, err := releases.Delete(h.DB, version)
		if err == nil && !deleted {
			return fail(http.StatusNotFound, "RELEASE_NOT_FOUND", "Release not found")
		}
		return err
	}()
	h.audit(c, "releases.delete", version, "", err)
	if err != nil {
		return nil, err
	}
	return gin.H{"deleted": true}, nil
}
