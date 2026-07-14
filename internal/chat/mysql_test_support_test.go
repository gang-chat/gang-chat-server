package chat

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"testing"
)

//go:embed testdata/mysql_schema.sql
var chatMySQLTestSchema string

func prepareChatMySQLTestDatabase(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := resetChatMySQLTestDatabase(context.Background(), db); err != nil {
		t.Fatalf("prepare MySQL test database: %v", err)
	}
}

func resetChatMySQLTestDatabase(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}

	var databaseName string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&databaseName); err != nil {
		return fmt.Errorf("read database name: %w", err)
	}
	if !isSafeMySQLTestDatabaseName(databaseName) {
		return fmt.Errorf("refusing to reset non-test database %q", databaseName)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve database connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		return fmt.Errorf("disable foreign key checks: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `SET FOREIGN_KEY_CHECKS = 1`) }()

	tables, err := mysqlTestTableNames(ctx, conn)
	if err != nil {
		return err
	}
	for _, table := range tables {
		quoted := "`" + strings.ReplaceAll(table, "`", "``") + "`"
		if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS `+quoted); err != nil {
			return fmt.Errorf("drop table %q: %w", table, err)
		}
	}

	for _, statement := range mysqlTestSchemaStatements(chatMySQLTestSchema) {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply test schema statement %q: %w", previewSQL(statement), err)
		}
	}
	return nil
}

func mysqlTestTableNames(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT TABLE_NAME
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, fmt.Errorf("list test tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("read test table name: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate test tables: %w", err)
	}
	return tables, nil
}

func isSafeMySQLTestDatabaseName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name != "" && (strings.HasSuffix(name, "_test") || strings.Contains(name, "_test_"))
}

func mysqlTestSchemaStatements(schema string) []string {
	lines := strings.Split(schema, "\n")
	keptLines := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		keptLines = append(keptLines, line)
	}
	parts := strings.Split(strings.Join(keptLines, "\n"), ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func previewSQL(statement string) string {
	const maxRunes = 120
	runes := []rune(strings.Join(strings.Fields(statement), " "))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}

func TestSafeMySQLTestDatabaseName(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"gang_chat_test":          true,
		"gang_chat_test_parallel": true,
		" GANG_CHAT_TEST ":        true,
		"gang_chat":               false,
		"contest":                 false,
		"test":                    false,
		"":                        false,
	}
	for name, want := range tests {
		name, want := name, want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isSafeMySQLTestDatabaseName(name); got != want {
				t.Fatalf("isSafeMySQLTestDatabaseName(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

func TestMySQLTestSchemaStatementsIgnoresCommentsAndEmptyStatements(t *testing.T) {
	t.Parallel()
	got := mysqlTestSchemaStatements("-- header; still a comment\nSET A = 1;\n\n-- middle\nCREATE TABLE example (id INT);;")
	want := []string{"SET A = 1", "CREATE TABLE example (id INT)"}
	if len(got) != len(want) {
		t.Fatalf("statements = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("statement %d = %q, want %q", index, got[index], want[index])
		}
	}
}
