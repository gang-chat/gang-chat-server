package idgen

import (
	"database/sql"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newSeqDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE id_sequences (name TEXT PRIMARY KEY NOT NULL, next_value INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	return db
}

func TestNextSeqMonotonicStart(t *testing.T) {
	db := newSeqDB(t)
	got := NextUserUID(db)
	if got != "10000000" {
		t.Fatalf("first uid = %q, want 10000000", got)
	}
	if got := NextUserUID(db); got != "10000001" {
		t.Fatalf("second uid = %q, want 10000001", got)
	}
	// Independent sequence.
	if got := NextRoomRID(db); got != "20000000" {
		t.Fatalf("first rid = %q, want 20000000", got)
	}
}

func TestNextSeqNoCollisionUnderConcurrency(t *testing.T) {
	db := newSeqDB(t)
	// SQLite serializes writers; one shared in-memory connection pool is enough
	// to exercise the read-modify-write atomicity of the UPSERT.
	db.SetMaxOpenConns(1)

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
			id := NextUserUID(db)
			mu.Lock()
			seen[id] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("got %d distinct ids, want %d (collision under concurrency)", len(seen), n)
	}
}
