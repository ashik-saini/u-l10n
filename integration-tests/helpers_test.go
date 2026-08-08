// Package integrationtests exercises the schema against a real PostgreSQL
// server.
//
// These tests apply .db/V*.sql directly rather than shelling out to Flyway, so
// they run anywhere Go and PostgreSQL are available. Flyway's own ability to
// apply the same files is verified separately (`make db-migrate`). What these
// tests prove is that the SQL is valid, correctly ordered, and — crucially —
// that the constraints reject bad input. A constraint nobody has watched
// reject anything is a constraint nobody can trust.
//
// The whole package is skipped when no database is reachable, so
// `go test ./...` stays green on a machine without PostgreSQL.
package integrationtests

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	_ "github.com/lib/pq"
)

const (
	envDatabaseURL     = "TEST_DATABASE_URL"
	defaultDatabaseURL = "postgres://localhost:5432/u_l10n_test?sslmode=disable"
)

// testDB is set by TestMain and shared by every test. Migrations are applied
// once; tests keep to their own key names rather than resetting between cases.
var testDB *sql.DB

func TestMain(m *testing.M) {
	dsn := os.Getenv(envDatabaseURL)
	if dsn == "" {
		dsn = defaultDatabaseURL
	}

	db, err := sql.Open("postgres", dsn)
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"integration-tests: skipping, no database at %s (%v)\n"+
				"  start one with: make db-test\n", dsn, err)
		os.Exit(0)
	}

	if err := applyMigrations(db); err != nil {
		fmt.Fprintf(os.Stderr, "integration-tests: %v\n", err)
		os.Exit(1)
	}

	testDB = db
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

// applyMigrations drops the public schema and replays every V*.sql in version
// order — the same "from empty" path Flyway takes on a fresh database.
func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(`DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}

	paths, err := filepath.Glob(filepath.Join("..", ".db", "V*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no migrations found in ../.db")
	}
	// Zero-padded minor versions mean lexical order equals Flyway's version
	// order. The V1.09 -> V1.10 transition is exactly why the padding exists.
	sort.Strings(paths)

	for _, path := range paths {
		stmt, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if _, err := db.Exec(string(stmt)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

// --- fixtures ---------------------------------------------------------------

var sortIndexCounter int64

// nextSortIndex hands out gapped sort indexes, mirroring how the importer seeds
// them so that inserting between two keys stays a single UPDATE.
func nextSortIndex() int64 {
	return atomic.AddInt64(&sortIndexCounter, 100)
}

// insertKey inserts a minimal active key and returns its id.
func insertKey(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	err := testDB.QueryRow(`
		INSERT INTO keys (name, platforms, sort_index)
		VALUES ($1, ARRAY['flutter']::TEXT[], $2)
		RETURNING id`, name, nextSortIndex()).Scan(&id)
	if err != nil {
		t.Fatalf("insert key %q: %v", name, err)
	}
	return id
}

// localeID resolves a locale code to its surrogate id.
func localeID(t *testing.T, code string) int16 {
	t.Helper()
	var id int16
	if err := testDB.QueryRow(`SELECT id FROM locales WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("locale %q: %v", code, err)
	}
	return id
}

// requireRejected asserts that a statement was refused by the database.
//
// The assertion is deliberately on the error, not on a row count: these tests
// exist to prove the constraint fires, and a silently-succeeding write is the
// exact failure they are written to catch.
func requireRejected(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the database to reject %s, but it was accepted", what)
	}
}
