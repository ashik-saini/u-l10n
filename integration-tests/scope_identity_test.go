package integrationtests

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExistingOperatorsKeepTheirAccessOnYouTrip proves the V1.13 backfill
// moved every role across rather than silently dropping people.
//
// The original form of this test compared count(users) to
// count(user_project_roles WHERE project_id = 1) directly. That comparison is
// unsound in this package: TestMain applies every migration ONCE, against an
// empty `users` table, so V1.13's backfill INSERT ... SELECT ... FROM users
// has nothing to select — the counts start at zero and zero. Any other test
// in this package that later inserts a user without cleaning it up (e.g.
// schema_test.go's TestEmailIsCaseInsensitive, which leaves
// 'Ashik.Saini@you.co' behind) breaks the equality through no fault of the
// migration: that user was never eligible for the one-time backfill, so it
// legitimately has no grant. Confirmed empirically — the original assertion
// passes when this file's tests are run alone and fails
// (expected 1, actual 0) under the full suite.
//
// This version drives the backfill's actual SQL directly — the exact
// statement V1.13 runs, re-executed against a user this test controls — which
// proves the logic without depending on what state other files left behind.
func TestExistingOperatorsKeepTheirAccessOnYouTrip(t *testing.T) {
	email := "scope-backfill@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'approver')`, email)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = $1`, email)
	})

	_, err = testDB.Exec(`
		INSERT INTO user_project_roles (email, project_id, role, granted_by)
		SELECT email, 1, role, 'migration:V1.13' FROM users
		ON CONFLICT (email, project_id) DO NOTHING`)
	require.NoError(t, err)

	var role string
	err = testDB.QueryRow(
		`SELECT role FROM user_project_roles WHERE email = $1 AND project_id = 1`, email).
		Scan(&role)
	require.NoError(t, err, "the backfill must grant every existing operator a YouTrip role")
	assert.Equal(t, "approver", role, "the grant must carry the role the user already had")

	var mismatched int
	require.NoError(t, testDB.QueryRow(`
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
	requireRejected(t, err, "unknown role")

	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-role@you.co', 999, 'viewer', 'test@you.co')`)
	requireRejected(t, err, "grant against a project that does not exist")
}

// TestUserProjectRoleGrantCascadesWithTheUser proves user_project_roles_email_fkey
// carries ON DELETE CASCADE: deleting a person must not be blocked by their own
// grants, which is why this FK is not NO ACTION the way the history tables are.
// A grant answers "what may this person do", not "what happened" — it has no
// reason to outlive the person it describes.
func TestUserProjectRoleGrantCascadesWithTheUser(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO users (email, role) VALUES ('scope-cascade@you.co', 'viewer')`)
	require.NoError(t, err)

	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-cascade@you.co', 1, 'viewer', 'test@you.co')`)
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
	enSG := localeID(t, "en-SG")

	t.Run("translation_history: YouTrip key with another project's locale", func(t *testing.T) {
		// All NOT NULL columns with no default supplied (key_id, locale_id,
		// version, source, changed_by) so the row reaches the FK rather than
		// being rejected at tuple formation for an unrelated reason.
		_, err := testDB.Exec(
			`INSERT INTO translation_history (project_id, key_id, locale_id, version, source, changed_by)
			 VALUES (1, $1, $2, 1, 'ui', 'test@you.co')`, youtripKey, otherLocale)
		requireRejected(t, err, "translation_history claiming a YouTrip key with another project's locale")
	})

	t.Run("translation_history: another project claiming a YouTrip key", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO translation_history (project_id, key_id, locale_id, version, source, changed_by)
			 VALUES ($1, $2, $3, 1, 'ui', 'test@you.co')`, otherProject, youtripKey, enSG)
		requireRejected(t, err, "another project's translation_history row claiming a YouTrip key")
	})

	t.Run("key_history: another project claiming a YouTrip key", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO key_history (project_id, key_id, name, platforms, status, version, source, changed_by)
			 VALUES ($1, $2, 'scope.test.history.key', ARRAY['flutter']::TEXT[], 'active', 1, 'ui', 'test@you.co')`,
			otherProject, youtripKey)
		requireRejected(t, err, "another project's key_history row claiming a YouTrip key")
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
		requireRejected(t, err, "an api_token against a project that does not exist")
	})

	t.Run("project_settings", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO project_settings (project_id, key, value, updated_by)
			 VALUES ($1, 'scope.test.setting', '{}'::JSONB, 'test@you.co')`, noSuchProject)
		requireRejected(t, err, "a project_settings row against a project that does not exist")
	})

	t.Run("audit_events", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO audit_events (project_id, actor, action)
			 VALUES ($1, 'test@you.co', 'scope.test.action')`, noSuchProject)
		requireRejected(t, err, "an audit_events row against a project that does not exist")
	})

	t.Run("import_runs", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO import_runs (project_id, started_by)
			 VALUES ($1, 'test@you.co')`, noSuchProject)
		requireRejected(t, err, "an import_runs row against a project that does not exist")
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
	requireRejected(t, err, "a duplicate key within the same project")
}
