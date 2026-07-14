package idgen

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func newSeqDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("GANG_TEST_IDGEN_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GANG_TEST_IDGEN_MYSQL_DSN is required for MySQL-backed idgen tests")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Match the production pool limit so the concurrency test exercises row
	// locking instead of exhausting the MySQL server's global connection cap.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	t.Cleanup(func() { _ = db.Close() })
	if err := requireIsolatedIDGenTestDatabase(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS id_sequences (name VARCHAR(64) PRIMARY KEY NOT NULL, next_value BIGINT NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (uid VARCHAR(128))`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS rooms (rid VARCHAR(128))`); err != nil {
		t.Fatalf("create rooms: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM id_sequences`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users`); err != nil {
		t.Fatalf("clear users: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM rooms`); err != nil {
		t.Fatalf("clear rooms: %v", err)
	}
	return db
}

func requireIsolatedIDGenTestDatabase(db *sql.DB) error {
	var name string
	if err := db.QueryRow(`SELECT DATABASE()`).Scan(&name); err != nil {
		return fmt.Errorf("read idgen test database name: %w", err)
	}
	if !isSafeIDGenTestDatabaseName(name) {
		return fmt.Errorf("refusing to modify non-test idgen database %q", name)
	}
	return nil
}

func isSafeIDGenTestDatabaseName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name != "" && (strings.HasSuffix(name, "_test") || strings.Contains(name, "_test_"))
}

func TestSafeIDGenTestDatabaseName(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"gang_chat_idgen_test": true,
		"idgen_test_parallel":  true,
		" GANG_TEST ":          true,
		"gang_chat":            false,
		"contest":              false,
		"test":                 false,
		"":                     false,
	}
	for name, want := range tests {
		name, want := name, want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isSafeIDGenTestDatabaseName(name); got != want {
				t.Fatalf("isSafeIDGenTestDatabaseName(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

func TestNextSeqMonotonicStart(t *testing.T) {
	db := newSeqDB(t)
	got, err := NextUserUID(db)
	if err != nil {
		t.Fatalf("first user uid: %v", err)
	}
	if got != "10000000" {
		t.Fatalf("first uid = %q, want 10000000", got)
	}
	if got, err := NextUserUID(db); err != nil || got != "10000001" {
		t.Fatalf("second uid = %q, want 10000001", got)
	}
	// Independent sequence.
	if got, err := NextRoomRID(db); err != nil || got != "20000000" {
		t.Fatalf("first rid = %q, want 20000000", got)
	}
}

func TestNextSeqNoCollisionUnderConcurrency(t *testing.T) {
	db := newSeqDB(t)

	const n = 500
	var (
		mu   sync.Mutex
		seen = make(map[string]struct{}, n)
		wg   sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := NextUserUID(db)
			mu.Lock()
			if err != nil {
				t.Errorf("allocate concurrent uid: %v", err)
				mu.Unlock()
				return
			}
			seen[id] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("got %d distinct ids, want %d (collision under concurrency)", len(seen), n)
	}
}

func TestNextSeqSeedsBeyondExistingPublicIDs(t *testing.T) {
	db := newSeqDB(t)
	if _, err := db.Exec(`INSERT INTO users (uid) VALUES ('10000005'), ('legacy-name')`); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms (rid) VALUES ('20000008'), ('room-old')`); err != nil {
		t.Fatalf("seed rooms: %v", err)
	}

	if uid, err := NextUserUID(db); err != nil || uid != "10000006" {
		t.Fatalf("seeded uid = %q, %v; want 10000006", uid, err)
	}
	if rid, err := NextRoomRID(db); err != nil || rid != "20000009" {
		t.Fatalf("seeded rid = %q, %v; want 20000009", rid, err)
	}
}

func TestNextUserUIDIgnoresReservedSuperUID(t *testing.T) {
	db := newSeqDB(t)
	if _, err := db.Exec(`INSERT INTO users (uid) VALUES (?)`, ReservedSuperUID); err != nil {
		t.Fatalf("seed super user: %v", err)
	}

	if uid, err := NextUserUID(db); err != nil || uid != "10000000" {
		t.Fatalf("uid after reserved super user = %q, %v; want 10000000", uid, err)
	}
}

func TestNextUserUIDRepairsSequencePoisonedByReservedSuperUID(t *testing.T) {
	db := newSeqDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (uid) VALUES ('10000005'), (?), ('66666667')`,
		ReservedSuperUID,
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO id_sequences (name, next_value) VALUES ('user_uid', 66666668)`,
	); err != nil {
		t.Fatalf("seed poisoned sequence: %v", err)
	}

	if uid, err := NextUserUID(db); err != nil || uid != "10000006" {
		t.Fatalf("repaired uid = %q, %v; want 10000006", uid, err)
	}
	if uid, err := NextUserUID(db); err != nil || uid != "10000007" {
		t.Fatalf("next repaired uid = %q, %v; want 10000007", uid, err)
	}
}

func TestNextUserUIDSkipsReservedAndOccupiedHighValues(t *testing.T) {
	db := newSeqDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (uid) VALUES ('66666665'), (?), ('66666667')`,
		ReservedSuperUID,
	); err != nil {
		t.Fatalf("seed boundary users: %v", err)
	}

	if uid, err := NextUserUID(db); err != nil || uid != "66666668" {
		t.Fatalf("boundary uid = %q, %v; want 66666668", uid, err)
	}
}

func TestNextSeqReturnsDatabaseErrorsInsteadOfDuplicateFallbacks(t *testing.T) {
	db, err := sql.Open("mysql", "")
	if err != nil {
		t.Fatalf("open closed test database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	if uid, err := NextUserUID(db); err == nil || uid != "" {
		t.Fatalf("closed database allocation = %q, %v; want empty uid and error", uid, err)
	}
	if _, err := NextUserUID(nil); err == nil {
		t.Fatal("nil database should return an error")
	}
}
