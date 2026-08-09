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
	// deleting the project while any of them still points at it would fail —
	// silently, since every cleanup below discards its error the same way
	// scope_constraints_test.go does, to avoid one cleanup's failure masking
	// the assertion that already ran.
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM locales WHERE id = $1`, otherLocale)
	})

	var otherBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES ($1, 'q3-copy-other', 'open', 'test@you.co') RETURNING id`,
		otherProject).Scan(&otherBranch))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE id = $1`, otherBranch)
	})

	var otherKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES ($1, 'branchscope.other.key', '{"flutter"}', 'active', $2)
		 RETURNING id`, otherProject, nextSortIndex()).Scan(&otherKey))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM keys WHERE id = $1`, otherKey)
	})

	var youtripBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'main-scope-branch', 'open', 'test@you.co') RETURNING id`).
		Scan(&youtripBranch))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE id = $1`, youtripBranch)
	})

	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'branchscope.youtrip.key', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
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
		requireRejected(t, err, "branch delta naming another project's key")
	})

	t.Run("branch_keys: another project's branch claiming a YouTrip-adjacent key delta", func(t *testing.T) {
		// (otherProject, otherKey) is a real key row: branch_keys_key_fkey is
		// satisfied. (otherProject, youtripBranch) is not — youtripBranch belongs
		// to project 1 — so only branch_keys_branch_fkey can refuse this row.
		_, err := testDB.Exec(
			`INSERT INTO branch_keys (project_id, branch_id, key_id, name, status, base_master_version, platforms, updated_by)
			 VALUES ($1, $2, $3, 'branchscope.key2', 'active', 0, '{"flutter"}', 'test@you.co')`,
			otherProject, youtripBranch, otherKey)
		requireRejected(t, err, "another project's branch claiming a key delta")
	})

	t.Run("branch_translations: value delta naming another project's key", func(t *testing.T) {
		// branch_fkey and locale_fkey are both satisfied ((otherProject, otherBranch)
		// and (otherProject, otherLocale) are real rows); only branch_translations_key_fkey
		// can refuse (otherProject, youtripKey).
		_, err := testDB.Exec(
			`INSERT INTO branch_translations (project_id, branch_id, key_id, locale_id, value, base_master_version, updated_by)
			 VALUES ($1, $2, $3, $4, 'x', 0, 'test@you.co')`,
			otherProject, otherBranch, youtripKey, otherLocale)
		requireRejected(t, err, "value delta naming another project's key")
	})

	t.Run("branch_translations: value delta naming another project's locale", func(t *testing.T) {
		// branch_fkey and key_fkey are both satisfied; only
		// branch_translations_locale_fkey can refuse (otherProject, youtripLocale).
		_, err := testDB.Exec(
			`INSERT INTO branch_translations (project_id, branch_id, key_id, locale_id, value, base_master_version, updated_by)
			 VALUES ($1, $2, $3, $4, 'x', 0, 'test@you.co')`,
			otherProject, otherBranch, otherKey, youtripLocale)
		requireRejected(t, err, "value delta naming another project's locale")
	})

	t.Run("branch_translations: another project's branch claiming a value delta", func(t *testing.T) {
		// key_fkey and locale_fkey are both satisfied; only
		// branch_translations_branch_fkey can refuse (otherProject, youtripBranch).
		_, err := testDB.Exec(
			`INSERT INTO branch_translations (project_id, branch_id, key_id, locale_id, value, base_master_version, updated_by)
			 VALUES ($1, $2, $3, $4, 'x', 0, 'test@you.co')`,
			otherProject, youtripBranch, otherKey, otherLocale)
		requireRejected(t, err, "another project's branch claiming a value delta")
	})
}

// TestBranchNamesAreUniquePerProjectNotGlobally: both teams may run a q3-copy.
func TestBranchNamesAreUniquePerProjectNotGlobally(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('namescope', 'Name Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	_, err := testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'shared-name', 'open', 'test@you.co')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE project_id = 1 AND name = 'shared-name'`)
	})

	// Registered after the project's cleanup, so it runs FIRST (t.Cleanup is
	// LIFO) — branches_project_fkey carries no ON DELETE CASCADE, so deleting
	// the project first would fail while this row still points at it.
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE project_id = $1 AND name = 'shared-name'`, otherProject)
	})

	_, err = testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES ($1, 'shared-name', 'open', 'test@you.co')`, otherProject)
	require.NoError(t, err, "the same branch name in another project must be allowed")

	_, err = testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'shared-name', 'open', 'test@you.co')`)
	requireRejected(t, err, "duplicate branch name within one project")
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
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	var youtripBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'mr-scope-branch', 'open', 'test@you.co') RETURNING id`).
		Scan(&youtripBranch))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE id = $1`, youtripBranch)
	})

	// (otherProject, youtripBranch) names a branch that exists, but not under
	// otherProject: only merge_requests_branch_fkey can refuse this row.
	_, err := testDB.Exec(
		`INSERT INTO merge_requests (project_id, branch_id, title, created_by)
		 VALUES ($1, $2, 'cross-project mr', 'test@you.co')`,
		otherProject, youtripBranch)
	requireRejected(t, err, "another project's merge request claiming a YouTrip branch")
}

// TestCrossProjectMergeConflictResolutionIsRefused: merge_conflict_resolutions
// gained project_id and a composite key_fkey in V1.11, while its
// merge_request_id foreign key stays a plain single column — a resolution is
// always read alongside its merge request, which already carries project_id,
// so there is no analogous pairing risk to close there.
func TestCrossProjectMergeConflictResolutionIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('conflictscope', 'Conflict Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'conflictscope.key', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
	})

	var youtripBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'conflict-scope-branch', 'open', 'test@you.co') RETURNING id`).
		Scan(&youtripBranch))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE id = $1`, youtripBranch)
	})

	var mrID int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO merge_requests (project_id, branch_id, title, created_by)
		 VALUES (1, $1, 'conflict scope mr', 'test@you.co') RETURNING id`,
		youtripBranch).Scan(&mrID))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM merge_requests WHERE id = $1`, mrID)
	})

	// mrID is a real merge request (merge_conflict_resolutions_merge_request_id_fkey
	// is satisfied), but (otherProject, youtripKey) is not a real key row: only
	// merge_conflict_resolutions_key_fkey can refuse this row.
	_, err := testDB.Exec(
		`INSERT INTO merge_conflict_resolutions (project_id, merge_request_id, key_id, resolution, resolved_by)
		 VALUES ($1, $2, $3, 'mine', 'test@you.co')`,
		otherProject, mrID, youtripKey)
	requireRejected(t, err, "another project's conflict resolution claiming a YouTrip key")
}
