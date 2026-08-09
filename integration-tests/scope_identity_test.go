package integrationtests

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopeIdentityScratchCounter gives each scratch database a unique name, so a
// -count>1 or repeated local run never collides with a database a previous
// run failed to clean up.
var scopeIdentityScratchCounter int64

// scopeIdentityScratchDB creates a throwaway database on the same server
// testDB is connected to, returns a handle to it, and registers cleanup that
// closes the handle and drops the database. It exists so
// TestExistingOperatorsKeepTheirAccessOnYouTrip can observe V1.13's backfill
// running against a `users` table seeded BEFORE the migration applies —
// something the shared, already-migrated testDB cannot do, since TestMain
// applies every migration once, up front, against an empty schema.
func scopeIdentityScratchDB(t *testing.T) *sql.DB {
	t.Helper()

	u, err := url.Parse(testDSN)
	require.NoError(t, err)
	name := fmt.Sprintf("scope_identity_scratch_%d", atomic.AddInt64(&scopeIdentityScratchCounter, 1))

	admin := *u
	admin.Path = "/postgres"
	adminDB, err := sql.Open("postgres", admin.String())
	require.NoError(t, err)
	defer adminDB.Close()

	_, err = adminDB.Exec(`CREATE DATABASE ` + name)
	require.NoError(t, err)
	t.Cleanup(func() {
		dropDB, dropErr := sql.Open("postgres", admin.String())
		if dropErr != nil {
			return
		}
		defer dropDB.Close()
		// WITH (FORCE) disconnects any lingering session on the scratch
		// database first; without it a connection this test forgot to close
		// would make the DROP fail silently-if-ignored.
		_, _ = dropDB.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})

	scratch := *u
	scratch.Path = "/" + name
	db, err := sql.Open("postgres", scratch.String())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// scopeIdentityApplyMigrationsExcept replays every .db/V*.sql file in version
// order into db, skipping the named file. Used to bring a scratch database up
// to "everything before V1.13", the state the real migration is meant to run
// against.
func scopeIdentityApplyMigrationsExcept(t *testing.T, db *sql.DB, exclude string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", ".db", "V*.sql"))
	require.NoError(t, err)
	sort.Strings(paths)
	require.NotEmpty(t, paths, "no migrations found in ../.db")

	for _, path := range paths {
		if filepath.Base(path) == exclude {
			continue
		}
		stmt, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = db.Exec(string(stmt))
		require.NoError(t, err, "applying %s", filepath.Base(path))
	}
}

// scopeIdentityApplyMigrationFile reads and executes exactly one .db/V*.sql
// file against db, returning whatever error Postgres gives back rather than
// asserting — callers that need to prove a mutated migration fails want the
// raw error, not a require.NoError that would abort the test before it can
// inspect it.
func scopeIdentityApplyMigrationFile(t *testing.T, db *sql.DB, name string) error {
	t.Helper()
	stmt, err := os.ReadFile(filepath.Join("..", ".db", name))
	require.NoError(t, err)
	_, err = db.Exec(string(stmt))
	return err
}

// TestExistingOperatorsKeepTheirAccessOnYouTrip proves the V1.13 backfill
// moved every role across rather than silently dropping people.
//
// This test used to compare count(users) to
// count(user_project_roles WHERE project_id = 1) on the shared testDB
// directly. Review caught two problems with that, both confirmed by hand:
//
//  1. TestMain applies every migration ONCE, against an empty `users` table
//     — no migration seeds a user — so V1.13's backfill INSERT selects zero
//     rows in that run. The original assertion was 0 == 0: vacuously true,
//     not a proof the backfill does anything.
//  2. A later rewrite replaced the comparison with an INLINE COPY of the
//     backfill statement, executed directly against a user the test itself
//     inserted. That made the assertion non-vacuous but stopped it from
//     testing the migration at all: deleting the backfill from V1.13
//     entirely, retargeting it at the wrong project, or hardcoding a role
//     would not change this test's result, because the test never reads
//     what V1.13 did — only what its own duplicate SQL did.
//
// This version seeds `users` with varied roles in a throwaway database
// BEFORE applying `.db/V1.13__scope_identity.sql` FROM DISK, then asserts on
// what that real file produced. Deleting or breaking the backfill in the
// actual migration file now changes this test's outcome — see the
// mutation-check evidence in the task report.
func TestExistingOperatorsKeepTheirAccessOnYouTrip(t *testing.T) {
	scratch := scopeIdentityScratchDB(t)
	scopeIdentityApplyMigrationsExcept(t, scratch, "V1.13__scope_identity.sql")

	// Varied roles, including an admin (must become a platform admin) and a
	// mixed-case CITEXT address (the backfill's SELECT must not silently
	// drop or duplicate it under case folding).
	seeded := []struct {
		email             string
		role              string
		wantPlatformAdmin bool
	}{
		{"Backfill.Admin@you.co", "admin", true},
		{"backfill.editor@you.co", "editor", false},
		{"BACKFILL.VIEWER@you.co", "viewer", false},
	}
	for _, s := range seeded {
		_, err := scratch.Exec(`INSERT INTO users (email, role) VALUES ($1, $2)`, s.email, s.role)
		require.NoError(t, err)
	}

	require.NoError(t, scopeIdentityApplyMigrationFile(t, scratch, "V1.13__scope_identity.sql"))

	for _, s := range seeded {
		t.Run(s.email, func(t *testing.T) {
			var role string
			var platformAdmin bool
			err := scratch.QueryRow(`
				SELECT r.role, u.is_platform_admin
				  FROM users u
				  JOIN user_project_roles r ON r.email = u.email AND r.project_id = 1
				 WHERE u.email = $1`, s.email).Scan(&role, &platformAdmin)
			require.NoError(t, err, "the backfill must grant every seeded operator a YouTrip role")
			assert.Equal(t, s.role, role, "the grant must carry the role the user already had")
			assert.Equal(t, s.wantPlatformAdmin, platformAdmin,
				"only an existing admin becomes a platform admin")
		})
	}

	var users, grants int
	require.NoError(t, scratch.QueryRow(`SELECT count(*) FROM users`).Scan(&users))
	require.NoError(t, scratch.QueryRow(
		`SELECT count(*) FROM user_project_roles WHERE project_id = 1`).Scan(&grants))
	assert.Equal(t, users, grants,
		"every seeded user must hold a YouTrip grant — this scratch database has no "+
			"other test's leftovers to pollute the count, unlike the shared testDB")

	var mismatched int
	require.NoError(t, scratch.QueryRow(`
		SELECT count(*) FROM users u
		  JOIN user_project_roles r ON r.email = u.email AND r.project_id = 1
		 WHERE r.role <> u.role`).Scan(&mismatched))
	assert.Zero(t, mismatched, "every grant must carry the role its user already had")
}

// TestRoleGrantsRequireAKnownRole: the ordered-role vocabulary is closed.
func TestRoleGrantsRequireAKnownRole(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO users (email, role) VALUES ('scope-role@you.co', 'viewer')
		 ON CONFLICT (email) DO NOTHING`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = 'scope-role@you.co'`)
	})

	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-role@you.co', 1, 'superuser', 'test@you.co')`)
	requireRejected(t, err, "user_project_roles_role_check",
		"unknown role")

	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-role@you.co', 999, 'viewer', 'test@you.co')`)
	requireRejected(t, err, "user_project_roles_project_id_fkey",
		"grant against a project that does not exist")
}

// TestUserProjectRoleGrantCascadesWithTheUser proves user_project_roles_email_fkey
// carries ON DELETE CASCADE: deleting a person must not be blocked by their own
// grants, which is why this FK is not NO ACTION the way the history tables are.
// A grant answers "what may this person do", not "what happened" — it has no
// reason to outlive the person it describes.
func TestUserProjectRoleGrantCascadesWithTheUser(t *testing.T) {
	// ON CONFLICT DO NOTHING plus a cleanup, even though the test's own last
	// act is to delete this user: any failure before that point leaks the row
	// permanently, and every later run would then fail on users_email_unique
	// instead of on whatever it was actually asserting — a red test that no
	// longer tells you anything.
	_, err := testDB.Exec(
		`INSERT INTO users (email, role) VALUES ('scope-cascade@you.co', 'viewer')
		 ON CONFLICT (email) DO NOTHING`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM users WHERE email = 'scope-cascade@you.co'`)
		require.NoError(t, cleanupErr)
	})

	// Same reasoning: a run that died after this INSERT leaked the grant along
	// with the user, so the pkey would be the thing failing next time round.
	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-cascade@you.co', 1, 'viewer', 'test@you.co')
		 ON CONFLICT (email, project_id) DO NOTHING`)
	require.NoError(t, err)

	_, err = testDB.Exec(`DELETE FROM users WHERE email = 'scope-cascade@you.co'`)
	require.NoError(t, err, "deleting the user must not be blocked by its own grant")

	var grants int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM user_project_roles WHERE email = 'scope-cascade@you.co'`).
		Scan(&grants))
	assert.Zero(t, grants, "the grant must go with the user, not survive as an orphan")
}

// TestHistoryCrossProjectPairingIsRefused proves the composite foreign keys
// added to translation_history and key_history do the same job V1.10 assigned
// to translations and key_tags: a single-column key_id cannot say "and it must
// be the same project", so without the composite pair nothing would stop a
// YouTrip key's audit row claiming another project's locale, or another
// project's history row claiming a YouTrip key.
func TestHistoryCrossProjectPairingIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('scopetest-hist', 'Scope Test History')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	// A locale belonging to the other project. Cleanup registered before the
	// project's, per the LIFO reasoning in scope_constraints_test.go: the
	// locale FK carries no ON DELETE CASCADE, so the project delete above
	// would otherwise fail, silently, if this ran second.
	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE id = $1`, otherLocale)
		require.NoError(t, cleanupErr)
	})

	// A key belonging to YouTrip.
	youtripKey := insertKey(t, "scope.test.history.key")
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
		require.NoError(t, cleanupErr)
	})

	t.Run("translation_history: YouTrip key with another project's locale", func(t *testing.T) {
		// project_id=1 matches youtripKey's project, so translation_history_key_fkey
		// is satisfied; (project_id, locale_id) = (1, otherLocale) is not a row in
		// locales, since otherLocale belongs to otherProject — only
		// translation_history_locale_fkey can be the one refusing this row.
		//
		// All NOT NULL columns with no default supplied (key_id, locale_id,
		// version, source, changed_by) so the row reaches the FK rather than
		// being rejected at tuple formation for an unrelated reason.
		_, err := testDB.Exec(
			`INSERT INTO translation_history (project_id, key_id, locale_id, version, source, changed_by)
			 VALUES (1, $1, $2, 1, 'ui', 'test@you.co')`, youtripKey, otherLocale)
		requireRejected(t, err, "translation_history_locale_fkey",
			"translation_history claiming a YouTrip key with another project's locale")
	})

	t.Run("translation_history: another project claiming a YouTrip key", func(t *testing.T) {
		// (project_id, locale_id) = (otherProject, otherLocale) IS a real row in
		// locales, so translation_history_locale_fkey is satisfied here.
		// (project_id, key_id) = (otherProject, youtripKey) is not — youtripKey
		// belongs to project 1 — so only translation_history_key_fkey can be the
		// one refusing this row.
		//
		// Using localeID(t, "en-SG") here instead of otherLocale was the original,
		// wrong form: that helper is unscoped (SELECT id FROM locales WHERE
		// code = $1, no project filter), and by this point in the test there are
		// TWO rows named 'en-SG' — YouTrip's own and the one this test just
		// inserted for otherProject. Whichever one it happened to return, the row
		// below would end up violating the locale_fkey as well as the key_fkey
		// (either the locale itself belongs to project 1, contradicting
		// project_id=otherProject, or — if it returned otherProject's own en-SG —
		// only the key mismatch would fire, which by luck happened to still be a
		// rejection). Dropping ONLY translation_history_key_fkey and leaving
		// translation_history_locale_fkey in place therefore still passed this
		// subtest, because the locale_fkey was quietly doing the rejecting
		// instead. otherLocale removes the ambiguity: it is unconditionally a
		// real row for otherProject, so this row can violate one and only one FK.
		_, err := testDB.Exec(
			`INSERT INTO translation_history (project_id, key_id, locale_id, version, source, changed_by)
			 VALUES ($1, $2, $3, 1, 'ui', 'test@you.co')`, otherProject, youtripKey, otherLocale)
		requireRejected(t, err, "translation_history_key_fkey",
			"another project's translation_history row claiming a YouTrip key")
	})

	t.Run("key_history: another project claiming a YouTrip key", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO key_history (project_id, key_id, name, platforms, status, version, source, changed_by)
			 VALUES ($1, $2, 'scope.test.history.key', ARRAY['flutter']::TEXT[], 'active', 1, 'ui', 'test@you.co')`,
			otherProject, youtripKey)
		requireRejected(t, err, "key_history_key_fkey",
			"another project's key_history row claiming a YouTrip key")
	})
}

// TestOperationalTablesRejectAnUnknownProject proves api_tokens, project_settings,
// audit_events and import_runs each got a real foreign key, not just a column —
// a project_id that names no row in projects must be refused by every one of
// them individually.
func TestOperationalTablesRejectAnUnknownProject(t *testing.T) {
	const noSuchProject = 999

	t.Run("api_tokens", func(t *testing.T) {
		hash := strings.Repeat("c", 64)
		_, err := testDB.Exec(
			`INSERT INTO api_tokens (project_id, name, token_sha256, token_prefix, created_by)
			 VALUES ($1, 'ci', $2, 'ul10n_test', 'test@you.co')`, noSuchProject, hash)
		requireRejected(t, err, "api_tokens_project_fkey",
			"an api_token against a project that does not exist")
	})

	t.Run("project_settings", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO project_settings (project_id, key, value, updated_by)
			 VALUES ($1, 'scope.test.setting', '{}'::JSONB, 'test@you.co')`, noSuchProject)
		requireRejected(t, err, "project_settings_project_fkey",
			"a project_settings row against a project that does not exist")
	})

	t.Run("audit_events", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO audit_events (project_id, actor, action)
			 VALUES ($1, 'test@you.co', 'scope.test.action')`, noSuchProject)
		requireRejected(t, err, "audit_events_project_fkey",
			"an audit_events row against a project that does not exist")
	})

	t.Run("import_runs", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO import_runs (project_id, started_by)
			 VALUES ($1, 'test@you.co')`, noSuchProject)
		requireRejected(t, err, "import_runs_project_fkey",
			"an import_runs row against a project that does not exist")
	})
}

// TestProjectSettingsKeyIsScopedPerProject proves the primary key move from
// (key) to (project_id, key) is real: the same setting key must now be usable
// independently by two different projects, and still unique within one.
func TestProjectSettingsKeyIsScopedPerProject(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('scopetest-settings', 'Scope Test Settings')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(
			`DELETE FROM project_settings WHERE project_id = $1`, otherProject)
		require.NoError(t, cleanupErr)
		_, cleanupErr = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(
			`DELETE FROM project_settings WHERE project_id = 1 AND key = 'scope.test.shared_key'`)
		require.NoError(t, cleanupErr)
	})

	_, err := testDB.Exec(
		`INSERT INTO project_settings (project_id, key, value, updated_by)
		 VALUES (1, 'scope.test.shared_key', '"youtrip"', 'test@you.co')`)
	require.NoError(t, err)

	_, err = testDB.Exec(
		`INSERT INTO project_settings (project_id, key, value, updated_by)
		 VALUES ($1, 'scope.test.shared_key', '"other"', 'test@you.co')`, otherProject)
	assert.NoError(t, err, "the same key must be usable independently by another project")

	_, err = testDB.Exec(
		`INSERT INTO project_settings (project_id, key, value, updated_by)
		 VALUES (1, 'scope.test.shared_key', '"duplicate"', 'test@you.co')`)
	requireRejected(t, err, "project_settings_pkey",
		"a duplicate key within the same project")
}
