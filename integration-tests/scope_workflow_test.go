package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCrossProjectBranchDeltaIsRefused proves that branch_keys and
// branch_translations — the two copy-on-write delta tables a branch is built
// from — cannot pair a branch, a key or a locale belonging to one project
// with a row belonging to another. Each subtest below holds two of the three
// dimensions (project_id, branch/key/locale) valid and only the third wrong,
// so a passing subtest can only be explained by the ONE composite foreign key
// it targets — the same isolation scope_constraints_test.go's key_tags test
// uses for V1.10's pair of composite keys.
//
// Every INSERT below supplies every NOT NULL column that carries no default:
// branch_keys needs platforms and updated_by, branch_translations needs value
// and updated_by, keys needs sort_index. Omitting any of them gets the row
// rejected with 23502 during tuple formation, before ANY foreign key is
// evaluated — which is exactly how a nearly identical test in an earlier task
// passed against a schema with zero cross-project protection.
func TestCrossProjectBranchDeltaIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('branchscope', 'Branch Scope')
		 RETURNING id`).Scan(&otherProject))
	// Registered first so it runs LAST (t.Cleanup is LIFO): branches_project_fkey,
	// keys_project_fkey and locales_project_fkey carry no ON DELETE CASCADE, so
	// deleting the project before the rows below would fail. Every cleanup here
	// asserts require.NoError, following scope_constraints_test.go's actual
	// pattern (not the discard-the-error version this file wrongly claimed
	// before review): a locale left behind by a swallowed error is exactly the
	// leak that makes schema_test.go's unscoped `count(*) FROM locales` and
	// helpers_test.go's unscoped `localeID` lookup depend on run order.
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, err)
	})

	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM locales WHERE id = $1`, otherLocale)
		require.NoError(t, err)
	})

	var otherBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES ($1, 'q3-copy-other', 'open', 'test@you.co') RETURNING id`,
		otherProject).Scan(&otherBranch))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM branches WHERE id = $1`, otherBranch)
		require.NoError(t, err)
	})

	var otherKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES ($1, 'branchscope.other.key', '{"flutter"}', 'active', $2)
		 RETURNING id`, otherProject, nextSortIndex()).Scan(&otherKey))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM keys WHERE id = $1`, otherKey)
		require.NoError(t, err)
	})

	var youtripBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'main-scope-branch', 'open', 'test@you.co') RETURNING id`).
		Scan(&youtripBranch))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM branches WHERE id = $1`, youtripBranch)
		require.NoError(t, err)
	})

	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'branchscope.youtrip.key', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
		require.NoError(t, err)
	})

	youtripLocale := localeID(t, "en-MY")

	t.Run("branch_keys: delta naming another project's key", func(t *testing.T) {
		// (otherProject, otherBranch) is a real branch row: branch_keys_branch_fkey
		// is satisfied. (otherProject, youtripKey) is not — youtripKey belongs to
		// project 1 — so only branch_keys_key_fkey can be the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO branch_keys (project_id, branch_id, key_id, name, status, base_master_version, platforms, updated_by)
			 VALUES ($1, $2, $3, 'branchscope.key', 'active', 0, '{"flutter"}', 'test@you.co')`,
			otherProject, otherBranch, youtripKey)
		requireRejected(t, err, "branch_keys_key_fkey",
			"branch delta naming another project's key")
	})

	t.Run("branch_keys: another project's branch claiming a YouTrip-adjacent key delta", func(t *testing.T) {
		// (otherProject, otherKey) is a real key row: branch_keys_key_fkey is
		// satisfied. (otherProject, youtripBranch) is not — youtripBranch belongs
		// to project 1 — so only branch_keys_branch_fkey can refuse this row.
		_, err := testDB.Exec(
			`INSERT INTO branch_keys (project_id, branch_id, key_id, name, status, base_master_version, platforms, updated_by)
			 VALUES ($1, $2, $3, 'branchscope.key2', 'active', 0, '{"flutter"}', 'test@you.co')`,
			otherProject, youtripBranch, otherKey)
		requireRejected(t, err, "branch_keys_branch_fkey",
			"another project's branch claiming a key delta")
	})

	t.Run("branch_translations: value delta naming another project's key", func(t *testing.T) {
		// branch_fkey and locale_fkey are both satisfied ((otherProject, otherBranch)
		// and (otherProject, otherLocale) are real rows); only branch_translations_key_fkey
		// can refuse (otherProject, youtripKey).
		_, err := testDB.Exec(
			`INSERT INTO branch_translations (project_id, branch_id, key_id, locale_id, value, base_master_version, updated_by)
			 VALUES ($1, $2, $3, $4, 'x', 0, 'test@you.co')`,
			otherProject, otherBranch, youtripKey, otherLocale)
		requireRejected(t, err, "branch_translations_key_fkey",
			"value delta naming another project's key")
	})

	t.Run("branch_translations: value delta naming another project's locale", func(t *testing.T) {
		// branch_fkey and key_fkey are both satisfied; only
		// branch_translations_locale_fkey can refuse (otherProject, youtripLocale).
		_, err := testDB.Exec(
			`INSERT INTO branch_translations (project_id, branch_id, key_id, locale_id, value, base_master_version, updated_by)
			 VALUES ($1, $2, $3, $4, 'x', 0, 'test@you.co')`,
			otherProject, otherBranch, otherKey, youtripLocale)
		requireRejected(t, err, "branch_translations_locale_fkey",
			"value delta naming another project's locale")
	})

	t.Run("branch_translations: another project's branch claiming a value delta", func(t *testing.T) {
		// key_fkey and locale_fkey are both satisfied; only
		// branch_translations_branch_fkey can refuse (otherProject, youtripBranch).
		_, err := testDB.Exec(
			`INSERT INTO branch_translations (project_id, branch_id, key_id, locale_id, value, base_master_version, updated_by)
			 VALUES ($1, $2, $3, $4, 'x', 0, 'test@you.co')`,
			otherProject, youtripBranch, otherKey, otherLocale)
		requireRejected(t, err, "branch_translations_branch_fkey",
			"another project's branch claiming a value delta")
	})
}

// TestBranchNamesAreUniquePerProjectNotGlobally: both teams may run a q3-copy.
func TestBranchNamesAreUniquePerProjectNotGlobally(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('namescope', 'Name Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, err)
	})

	_, err := testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'shared-name', 'open', 'test@you.co')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM branches WHERE project_id = 1 AND name = 'shared-name'`)
		require.NoError(t, err)
	})

	// Registered after the project's cleanup, so it runs FIRST (t.Cleanup is
	// LIFO) — branches_project_fkey carries no ON DELETE CASCADE, so deleting
	// the project first would fail while this row still points at it.
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM branches WHERE project_id = $1 AND name = 'shared-name'`, otherProject)
		require.NoError(t, err)
	})

	_, err = testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES ($1, 'shared-name', 'open', 'test@you.co')`, otherProject)
	require.NoError(t, err, "the same branch name in another project must be allowed")

	_, err = testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'shared-name', 'open', 'test@you.co')`)
	requireRejected(t, err, "branches_project_name_unique",
		"duplicate branch name within one project")
}

// TestCrossProjectMergeRequestIsRefused: a merge request carries project_id
// directly (see V1.11's comment on why), and merge_requests_branch_fkey must
// refuse a project claiming a branch it does not own even though the branch
// row itself is perfectly valid.
func TestCrossProjectMergeRequestIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('mrscope', 'MR Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, err)
	})

	var youtripBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'mr-scope-branch', 'open', 'test@you.co') RETURNING id`).
		Scan(&youtripBranch))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM branches WHERE id = $1`, youtripBranch)
		require.NoError(t, err)
	})

	// (otherProject, youtripBranch) names a branch that exists, but not under
	// otherProject: only merge_requests_branch_fkey can refuse this row.
	_, err := testDB.Exec(
		`INSERT INTO merge_requests (project_id, branch_id, title, created_by)
		 VALUES ($1, $2, 'cross-project mr', 'test@you.co')`,
		otherProject, youtripBranch)
	requireRejected(t, err, "merge_requests_branch_fkey",
		"another project's merge request claiming a YouTrip branch")
}

// TestCrossProjectMergeConflictResolutionIsRefused: merge_conflict_resolutions
// gained project_id and composite key_fkey/locale_fkey in V1.11.
//
// merge_request_id stays a plain single-column foreign key. That IS a real
// pairing gap — a project-2 resolution can still be inserted against a
// project-1 merge request, the mirror image of what key_fkey and locale_fkey
// now close — but closing it needs `UNIQUE (project_id, id)` on
// merge_requests, a larger change deliberately deferred to the next plan.
// This test does not cover that gap; it exists, it is not absent.
func TestCrossProjectMergeConflictResolutionIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('conflictscope', 'Conflict Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, err)
	})

	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM locales WHERE id = $1`, otherLocale)
		require.NoError(t, err)
	})

	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'conflictscope.key', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
		require.NoError(t, err)
	})

	var youtripBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'conflict-scope-branch', 'open', 'test@you.co') RETURNING id`).
		Scan(&youtripBranch))
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM branches WHERE id = $1`, youtripBranch)
		require.NoError(t, err)
	})

	var mrID int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO merge_requests (project_id, branch_id, title, created_by)
		 VALUES (1, $1, 'conflict scope mr', 'test@you.co') RETURNING id`,
		youtripBranch).Scan(&mrID))
	// merge_conflict_resolutions_merge_request_id_fkey carries ON DELETE
	// CASCADE, so deleting this merge request also removes the NULL-locale
	// row the third subtest below actually persists — no separate cleanup
	// needed for that row.
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM merge_requests WHERE id = $1`, mrID)
		require.NoError(t, err)
	})

	t.Run("resolution claiming a YouTrip key from another project", func(t *testing.T) {
		// mrID is a real merge request (merge_conflict_resolutions_merge_request_id_fkey
		// is satisfied), but (otherProject, youtripKey) is not a real key row: only
		// merge_conflict_resolutions_key_fkey can refuse this row.
		_, err := testDB.Exec(
			`INSERT INTO merge_conflict_resolutions (project_id, merge_request_id, key_id, resolution, resolved_by)
			 VALUES ($1, $2, $3, 'mine', 'test@you.co')`,
			otherProject, mrID, youtripKey)
		requireRejected(t, err, "merge_conflict_resolutions_key_fkey",
			"another project's conflict resolution claiming a YouTrip key")
	})

	t.Run("resolution naming another project's locale is refused", func(t *testing.T) {
		// project_id=1 pairs correctly with mrID and youtripKey; only
		// merge_conflict_resolutions_locale_fkey can refuse (1, otherLocale) —
		// otherLocale belongs to otherProject, not project 1.
		_, err := testDB.Exec(
			`INSERT INTO merge_conflict_resolutions (project_id, merge_request_id, key_id, locale_id, resolution, resolved_by)
			 VALUES (1, $1, $2, $3, 'mine', 'test@you.co')`,
			mrID, youtripKey, otherLocale)
		requireRejected(t, err, "merge_conflict_resolutions_locale_fkey",
			"conflict resolution naming another project's locale")
	})

	t.Run("a NULL-locale metadata resolution still inserts", func(t *testing.T) {
		// The whole point of leaving locale_id nullable: Postgres's default
		// MATCH SIMPLE skips FK enforcement when ANY column of a composite key
		// is NULL. project_id is NOT NULL here, so only locale_id being NULL can
		// trigger that skip — proving the composite FK protects value-conflict
		// rows without also silently blocking every metadata-conflict row. If
		// this subtest failed instead of the one above, the "fix" would have
		// been to make locale_id NOT NULL, which is the wrong migration.
		_, err := testDB.Exec(
			`INSERT INTO merge_conflict_resolutions (project_id, merge_request_id, key_id, resolution, resolved_by)
			 VALUES (1, $1, $2, 'mine', 'test@you.co')`,
			mrID, youtripKey)
		require.NoError(t, err, "a metadata conflict resolution (NULL locale_id) must still be accepted")
	})
}
