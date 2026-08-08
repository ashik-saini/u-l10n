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
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/yougroupteam/u-common-components/database"
)

const (
	envDatabaseURL     = "TEST_DATABASE_URL"
	defaultDatabaseURL = "postgres://localhost:5432/u_l10n_test?sslmode=disable"
)

// testDB is set by TestMain and shared by every test. Migrations are applied
// once; tests keep to their own key names rather than resetting between cases.
var testDB *sql.DB

// testDSN is the resolved connection string, kept so the service-level tests
// can open a GORM handle of their own. The repositories speak GORM; driving
// them through database/sql would be testing a different code path from the one
// that runs in production.
var testDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()

	dsn, cleanup, err := provisionDatabase(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"integration-tests: skipping, no database available (%v)\n"+
				"  provide one with: make db-test, or set %s, or start Docker\n", err, envDatabaseURL)
		os.Exit(0)
	}

	db, err := sql.Open("postgres", dsn)
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration-tests: cannot reach %s: %v\n", dsn, err)
		cleanup()
		os.Exit(1)
	}

	if err := applyMigrations(db); err != nil {
		fmt.Fprintf(os.Stderr, "integration-tests: %v\n", err)
		_ = db.Close()
		cleanup()
		os.Exit(1)
	}

	testDB = db
	testDSN = dsn
	code := m.Run()

	_ = db.Close()
	cleanup()
	os.Exit(code)
}

// provisionDatabase resolves a PostgreSQL to test against, in priority order:
//
//  1. TEST_DATABASE_URL, when set — an explicit override always wins, so a
//     developer or CI can point at an existing server.
//  2. A testcontainers-managed container, when a Docker daemon is reachable.
//     This is the hermetic path: a throwaway server per run, nothing shared
//     between runs, nothing left behind.
//  3. The local development database, if one happens to be listening.
//
// Falling back rather than requiring Docker keeps `go test ./...` working on a
// machine without it — the suite skips instead of failing, which is the correct
// behaviour for a dependency the test author cannot install for you.
func provisionDatabase(ctx context.Context) (dsn string, cleanup func(), err error) {
	noop := func() {}

	if explicit := os.Getenv(envDatabaseURL); explicit != "" {
		return explicit, noop, nil
	}

	// postgres:15.3-alpine matches u-reward's integration tests, so a failure
	// here is a failure of our SQL rather than of a version difference.
	container, err := postgres.Run(ctx, "postgres:15.3-alpine",
		postgres.WithDatabase("u_l10n_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			// Twice: Postgres logs readiness once during its own init and again
			// when it opens for real connections. Waiting for the first would
			// race the server's restart.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second)),
	)
	if err == nil {
		dsn, dsnErr := container.ConnectionString(ctx, "sslmode=disable")
		if dsnErr == nil {
			return dsn, func() {
				if err := testcontainers.TerminateContainer(container); err != nil {
					fmt.Fprintf(os.Stderr, "integration-tests: terminate container: %v\n", err)
				}
			}, nil
		}
		_ = testcontainers.TerminateContainer(container)
		return "", noop, dsnErr
	}
	containerErr := err

	// No Docker. Fall back to a local server if one is listening.
	if probe, probeErr := sql.Open("postgres", defaultDatabaseURL); probeErr == nil {
		if probe.Ping() == nil {
			_ = probe.Close()
			return defaultDatabaseURL, noop, nil
		}
		_ = probe.Close()
	}

	return "", noop, fmt.Errorf("no container (%v) and no local database at %s",
		containerErr, defaultDatabaseURL)
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

// --- driving the real repository and service layers -------------------------

// gormConnector satisfies database.GORMConnector over a plain GORM handle.
//
// The production connector carries dynamic IAM credentials, APM instrumentation
// and a refresh goroutine, none of which a test can or should stand up. The
// interface is two methods wide, which is exactly why the repositories depend on
// it rather than on a *gorm.DB — the same move as route.Pinger and
// assetsvc.ObjectStore.
type gormConnector struct{ db *gorm.DB }

func (c gormConnector) GetDB() *gorm.DB { return c.db }

// GetDBWithContext returns the same handle. GORM v1 has no per-request context
// binding; the production connector does the same thing.
func (c gormConnector) GetDBWithContext(context.Context) *gorm.DB { return c.db }

var (
	gormOnce   sync.Once
	gormHandle *gorm.DB
	gormErr    error
)

// testGORM opens a GORM handle against the same database the schema tests use.
//
// Opened once for the whole package: gorm.Open builds a connection pool, and one
// per test would exhaust max_connections long before the suite finished.
func testGORM(t *testing.T) database.GORMConnector {
	t.Helper()

	gormOnce.Do(func() {
		gormHandle, gormErr = gorm.Open("postgres", testDSN)
		if gormErr == nil {
			gormHandle.DB().SetMaxOpenConns(8)
		}
	})
	if gormErr != nil {
		t.Fatalf("open gorm handle: %v", gormErr)
	}
	return gormConnector{db: gormHandle}
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
