package admin

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// The console browses the database read-only: tables, their columns and pages
// of rows. Changes go through the account and release endpoints, which keep the
// rows the way the server writes them.

type tableSummary struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

type tableColumn struct {
	Field        string `json:"field"`
	Type         string `json:"type"`
	Nullable     bool   `json:"nullable"`
	Key          string `json:"key"`
	DefaultValue any    `json:"defaultValue"`
	Extra        string `json:"extra"`
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (h *Handler) tableNames() ([]string, error) {
	rows, err := h.DB.Query(
		`SELECT name FROM sqlite_schema
		 WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// requireTable returns the table named in the URL, refusing names that aren't
// tables so they never reach the SQL text.
func (h *Handler) requireTable(c *gin.Context) (string, error) {
	names, err := h.tableNames()
	if err != nil {
		return "", err
	}
	table := c.Param("table")
	for _, name := range names {
		if name == table {
			return name, nil
		}
	}
	return "", fail(http.StatusNotFound, "TABLE_NOT_FOUND", "Table not found")
}

func (h *Handler) listTables(*gin.Context) (any, error) {
	names, err := h.tableNames()
	if err != nil {
		return nil, err
	}
	tables := make([]tableSummary, 0, len(names))
	for _, name := range names {
		summary := tableSummary{Name: name}
		if err := h.DB.QueryRow(`SELECT COUNT(*) FROM ` + quoteIdentifier(name)).Scan(&summary.Rows); err != nil {
			return nil, err
		}
		tables = append(tables, summary)
	}
	return tables, nil
}

func (h *Handler) describeTable(c *gin.Context) (any, error) {
	table, err := h.requireTable(c)
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Query(`SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := []tableColumn{}
	for rows.Next() {
		var column tableColumn
		var notNull, pk, hidden int
		if err := rows.Scan(&column.Field, &column.Type, &notNull, &column.DefaultValue, &pk, &hidden); err != nil {
			return nil, err
		}
		column.Nullable = notNull == 0 && pk == 0
		if pk > 0 {
			column.Key = "PRI"
		}
		if hidden != 0 {
			column.Extra = "generated"
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func (h *Handler) readRows(c *gin.Context) (any, error) {
	table, err := h.requireTable(c)
	if err != nil {
		return nil, err
	}
	limit := clampQueryInt(c, "limit", 100, 1, 500)
	offset := clampQueryInt(c, "offset", 0, 0, 1<<31-1)
	rows, err := h.DB.Query(`SELECT * FROM `+quoteIdentifier(table)+` LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(names))
		for i, name := range names {
			if blob, ok := values[i].([]byte); ok {
				row[name] = base64.StdEncoding.EncodeToString(blob)
			} else {
				row[name] = values[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func clampQueryInt(c *gin.Context, name string, fallback, low, high int) int {
	value, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return fallback
	}
	return min(max(value, low), high)
}
