// Package releases stores the client versions the admin console maintains and
// serves the published ones to the homepage and to /app/version.
package releases

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	datePattern    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// ErrNotFound is returned for a version that has no row.
var ErrNotFound = errors.New("release not found")

type Downloads struct {
	Windows string `json:"windows"`
	MacOS   string `json:"macos"`
	Android string `json:"android"`
}

type Release struct {
	Version                 string    `json:"version"`
	Title                   string    `json:"title"`
	Notes                   string    `json:"notes"`
	ReleaseDate             string    `json:"release_date"`
	Published               bool      `json:"published"`
	MinimumSupportedVersion string    `json:"minimum_supported_version"`
	Downloads               Downloads `json:"downloads"`
	CreatedAt               int64     `json:"created_at"`
	UpdatedAt               int64     `json:"updated_at"`
}

// ValidVersion reports whether v is a plain x.y.z version, the only form the
// clients and the release file names use.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// ValidDate reports whether d is empty or a YYYY-MM-DD date.
func ValidDate(d string) bool {
	if d == "" {
		return true
	}
	if !datePattern.MatchString(d) {
		return false
	}
	_, err := time.Parse("2006-01-02", d)
	return err == nil
}

// CompareVersions orders x.y.z versions numerically.
func CompareVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(left), len(right)); i++ {
		var l, r int
		if i < len(left) {
			l, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			r, _ = strconv.Atoi(right[i])
		}
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
}

const columns = `version, title, notes, release_date, published, minimum_supported_version,
	windows_url, macos_url, android_url, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (Release, error) {
	var r Release
	var published int
	err := row.Scan(&r.Version, &r.Title, &r.Notes, &r.ReleaseDate, &published, &r.MinimumSupportedVersion,
		&r.Downloads.Windows, &r.Downloads.MacOS, &r.Downloads.Android, &r.CreatedAt, &r.UpdatedAt)
	r.Published = published != 0
	return r, err
}

// List returns releases newest version first; publishedOnly leaves out drafts.
func List(db *sql.DB, publishedOnly bool) ([]Release, error) {
	query := `SELECT ` + columns + ` FROM app_releases`
	if publishedOnly {
		query += ` WHERE published = 1`
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Release{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return CompareVersions(list[i].Version, list[j].Version) > 0 })
	return list, nil
}

// Latest returns the newest published release, or nil when there is none.
func Latest(db *sql.DB) (*Release, error) {
	list, err := List(db, true)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func Get(db querier, version string) (*Release, error) {
	r, err := scan(db.QueryRow(`SELECT `+columns+` FROM app_releases WHERE version = ?`, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Save inserts r or replaces the row with its version, keeping created_at.
func Save(db execer, r Release) error {
	now := time.Now().UnixMilli()
	published := 0
	if r.Published {
		published = 1
	}
	_, err := db.Exec(
		`INSERT INTO app_releases (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (version) DO UPDATE SET
		   title = excluded.title, notes = excluded.notes, release_date = excluded.release_date,
		   published = excluded.published, minimum_supported_version = excluded.minimum_supported_version,
		   windows_url = excluded.windows_url, macos_url = excluded.macos_url,
		   android_url = excluded.android_url, updated_at = excluded.updated_at`,
		r.Version, r.Title, r.Notes, r.ReleaseDate, published, r.MinimumSupportedVersion,
		r.Downloads.Windows, r.Downloads.MacOS, r.Downloads.Android, now, now,
	)
	return err
}

func Delete(db execer, version string) (bool, error) {
	result, err := db.Exec(`DELETE FROM app_releases WHERE version = ?`, version)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// RegisterPublicRoutes serves the published releases without sign-in, for the
// homepage.
func RegisterPublicRoutes(g *gin.RouterGroup, db *sql.DB) {
	g.GET("/releases", func(c *gin.Context) {
		list, err := List(db, true)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "query failed"}})
			return
		}
		var latest *Release
		if len(list) > 0 {
			latest = &list[0]
		}
		c.Header("Cache-Control", "no-cache")
		c.JSON(http.StatusOK, gin.H{"latest": latest, "releases": list})
	})
}
