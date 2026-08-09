package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// Key-metadata conflicts are the SECOND conflict type — anchored on
// keys.version rather than translations.version — and name collisions are the
// THIRD, the only one that is not a version comparison at all.

// setKeyMeta mirrors setKeyMetaSQL, capturing base_master_version on first
// touch only.
func setKeyMeta(t *testing.T, branchID, keyID int64, name, status string) {
	t.Helper()
	_, err := testDB.Exec(`
		INSERT INTO branch_keys
		    (branch_id, key_id, name, platforms, status, base_master_version, updated_by)
		VALUES ($1, $2, $3, ARRAY['flutter']::TEXT[], $4,
		        COALESCE((SELECT version FROM keys WHERE id = $2), 0), 'test@you.co')
		ON CONFLICT (branch_id, key_id) DO UPDATE SET
		    name = EXCLUDED.name, status = EXCLUDED.status, updated_at = now()`,
		branchID, keyID, name, status)
	require.NoError(t, err)
}

// TestBranchKeysRefusesAKeylessDelta is the schema half of the silent-drop fix,
// and the test that discriminates the old model from the new one.
//
// branch_keys.key_id used to be nullable, to express "a key created on this
// branch that does not exist on master". Nothing could ever fill that state —
// branch_translations.key_id is NOT NULL REFERENCES keys (id), so such a key
// could carry no values — and applyKeyMetaSQL joins bk.key_id = k.id, so the
// row matched nothing, was skipped, and THE MERGE REPORTED SUCCESS HAVING
// DROPPED THE KEY. Silent, successful-looking data loss.
//
// Against the old schema this INSERT is accepted. It must now be refused: a key
// created on a branch is a real (draft) keys row from the moment it exists, so
// there is no longer any state for the merge to skip.
func TestBranchKeysRefusesAKeylessDelta(t *testing.T) {
	branchID := newBranch(t, "keyless-delta")

	_, err := testDB.Exec(`
		INSERT INTO branch_keys
		    (branch_id, key_id, name, platforms, status, base_master_version, updated_by)
		VALUES ($1, NULL, 'keyless_delta', ARRAY['flutter']::TEXT[], 'active', 0, 'test@you.co')`,
		branchID)
	// V1.07 made branch_keys.key_id NOT NULL, and a NOT NULL is the one
	// class-23 rejection Postgres attaches no constraint name to — hence the
	// column-asserting variant. Asserting the column is what stops this test
	// going green on some other omitted NOT NULL column instead.
	requireRejectedNotNull(t, err, "key_id", "a branch_keys delta naming no key")
}

// TestBranchTranslationsCannotOutrunTheirKey states the constraint that shapes
// the whole fix, in the schema's own words.
//
// It is the reason materialising branch-created keys at merge time would have
// completed a path nobody could use: without a keys row there can be no value,
// so a keyless delta could only ever have merged an EMPTY key.
func TestBranchTranslationsCannotOutrunTheirKey(t *testing.T) {
	branchID := newBranch(t, "value-without-key")
	enSG := localeID(t, "en-SG")

	var maxKeyID int64
	require.NoError(t, testDB.QueryRow(`SELECT COALESCE(max(id), 0) + 1 FROM keys`).Scan(&maxKeyID))

	_, err := testDB.Exec(`
		INSERT INTO branch_translations
		    (branch_id, key_id, locale_id, value, base_master_version, updated_by)
		VALUES ($1, $2, $3, 'orphan', 0, 'test@you.co')`, branchID, maxKeyID, enSG)
	requireRejected(t, err, "branch_translations_key_fkey",
		"a branch value against a key that does not exist")
}

func metaConflictCount(t *testing.T, branchID int64) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`
		SELECT count(*)
		  FROM branch_keys bk
		  JOIN keys k ON k.id = bk.key_id
		 WHERE bk.branch_id = $1 AND bk.key_id IS NOT NULL
		   AND COALESCE(k.version, 0) <> bk.base_master_version`, branchID).Scan(&n))
	return n
}

// TestMetaConflictAnchorsOnKeyVersion proves the second conflict type uses a
// different anchor from the first. A rename racing a rename is as much a
// conflict as two edits to one value, and keys.version is what detects it.
func TestMetaConflictAnchorsOnKeyVersion(t *testing.T) {
	branchID := newBranch(t, "meta-conflict")
	keyID := insertKey(t, "meta_conflict_original")

	setKeyMeta(t, branchID, keyID, "meta_conflict_renamed_by_branch", "active")
	assert.Zero(t, metaConflictCount(t, branchID), "master untouched: no conflict")

	// Master renames the same key independently.
	_, err := testDB.Exec(`
		UPDATE keys SET name = 'meta_conflict_renamed_by_master', version = version + 1
		 WHERE id = $1`, keyID)
	require.NoError(t, err)

	assert.Equal(t, 1, metaConflictCount(t, branchID),
		"rename/rename must conflict; keys.version is the anchor, not translations.version")

	// base_master_version must survive a later branch edit, exactly as for values.
	setKeyMeta(t, branchID, keyID, "meta_conflict_renamed_again", "active")
	var base int
	require.NoError(t, testDB.QueryRow(
		`SELECT base_master_version FROM branch_keys WHERE branch_id=$1 AND key_id=$2`,
		branchID, keyID).Scan(&base))
	assert.Equal(t, 1, base,
		"base_master_version records the starting point, not the latest edit")
}

// TestSoftDeleteOnBranchMergesAsAStatusChange proves a branch can delete a key
// and have that reach master through the metadata path — driven through the
// REAL ApplyKeyMeta, version guard and all, rather than a mirror of its SQL.
func TestSoftDeleteOnBranchMergesAsAStatusChange(t *testing.T) {
	ctx := context.Background()
	mrs := repository.ProvideMergeRequestRepository(testGORM(t))

	branchID := newBranch(t, "meta-softdelete")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "meta_softdelete_case")

	setKeyMeta(t, branchID, keyID, "meta_softdelete_case", "deleted")

	applied, blocked, err := mrs.ApplyKeyMeta(ctx, nil, mrID, branchID, "merger@you.co")
	require.NoError(t, err)
	assert.Zero(t, blocked, "master never moved, so nothing trips the guard")
	assert.Equal(t, 1, applied)

	var status string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM keys WHERE id = $1`, keyID).Scan(&status))
	assert.Equal(t, "deleted", status, "a branch soft delete must reach master")

	// The applied change is on the key's timeline: source 'merge', the
	// post-change state, attributed to the merging actor.
	var (
		histStatus, histSource, histActor string
	)
	require.NoError(t, testDB.QueryRow(`
		SELECT status, source, changed_by FROM key_history
		 WHERE key_id = $1 ORDER BY id DESC LIMIT 1`, keyID).
		Scan(&histStatus, &histSource, &histActor))
	assert.Equal(t, "deleted", histStatus)
	assert.Equal(t, "merge", histSource)
	assert.Equal(t, "merger@you.co", histActor)

	// And the name becomes reusable, because the unique index is partial.
	_, err = testDB.Exec(`
		INSERT INTO keys (name, platforms, sort_index)
		VALUES ('meta_softdelete_case', ARRAY['flutter']::TEXT[], $1)`, nextSortIndex())
	assert.NoError(t, err, "soft-deleting frees the name")
}

// TestNameCollisionIsDetectedBeforeApplying is the third conflict type.
//
// Both sides are internally consistent; it is the UNION that is impossible,
// because idx_keys_name_active permits one active key per name. Detecting it up
// front is what stops the merge dying on the unique index halfway through
// applying changes — a partial merge is far worse than a refused one.
func TestNameCollisionIsDetectedBeforeApplying(t *testing.T) {
	branchID := newBranch(t, "meta-collision")
	mine := insertKey(t, "collision_branch_key")
	theirs := insertKey(t, "collision_master_key")

	collisions := func() int {
		var n int
		require.NoError(t, testDB.QueryRow(`
			SELECT count(*)
			  FROM branch_keys bk
			  JOIN keys k ON k.name = bk.name AND k.status = 'active'
			 WHERE bk.branch_id = $1 AND bk.status = 'active'
			   AND (bk.key_id IS NULL OR bk.key_id <> k.id)`, branchID).Scan(&n))
		return n
	}

	// A delta that keeps the key's own name is not a collision with itself.
	setKeyMeta(t, branchID, mine, "collision_branch_key", "active")
	assert.Zero(t, collisions(), "a key must not collide with itself")

	// Renaming onto a name another ACTIVE master key holds is a collision.
	setKeyMeta(t, branchID, mine, "collision_master_key", "active")
	assert.Equal(t, 1, collisions(), "renaming onto an occupied active name collides")

	// Once master's holder is soft-deleted the name is free and the collision
	// disappears — the same partial-index semantics as everywhere else.
	_, err := testDB.Exec(`UPDATE keys SET status = 'deleted' WHERE id = $1`, theirs)
	require.NoError(t, err)
	assert.Zero(t, collisions(), "a soft-deleted holder frees the name")
}

// TestMetaResolutionMasterDiscardsBranchRename mirrors the value case: choosing
// master must DISCARD the branch's rename rather than apply it.
func TestMetaResolutionMasterDiscardsBranchRename(t *testing.T) {
	branchID := newBranch(t, "meta-resolution")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "meta_resolution_original")

	setKeyMeta(t, branchID, keyID, "meta_resolution_branch_name", "active")

	_, err := testDB.Exec(`
		UPDATE keys SET name = 'meta_resolution_master_name', version = version + 1
		 WHERE id = $1`, keyID)
	require.NoError(t, err)

	// The reviewer keeps master's name. locale_id IS NULL marks a metadata
	// conflict.
	_, err = testDB.Exec(`
		INSERT INTO merge_conflict_resolutions
		    (merge_request_id, key_id, locale_id, resolution, resolved_by)
		VALUES ($1, $2, NULL, 'master', 'approver@you.co')`, mrID, keyID)
	require.NoError(t, err)

	applied, blocked, err := repository.ProvideMergeRequestRepository(testGORM(t)).
		ApplyKeyMeta(context.Background(), nil, mrID, branchID, "merger@you.co")
	require.NoError(t, err)
	assert.Zero(t, blocked, "a decided conflict is an intentional skip, not a blocked delta")
	assert.Zero(t, applied, "the skipped delta must not count as applied")

	var name string
	require.NoError(t, testDB.QueryRow(`SELECT name FROM keys WHERE id = $1`, keyID).Scan(&name))
	assert.Equal(t, "meta_resolution_master_name", name,
		"resolution 'master' must DISCARD the branch rename, not apply it")
}

// TestApplyKeyMetaRefusesAConcurrentMasterWrite is the metadata half of the
// lost-update guard: a delta whose keys.version moved past base_master_version
// with NO resolution row must be counted as blocked and NOT applied, so the
// merge can refuse with ErrConcurrentMasterWrite instead of silently
// overwriting a rename somebody just committed.
func TestApplyKeyMetaRefusesAConcurrentMasterWrite(t *testing.T) {
	ctx := context.Background()
	mrs := repository.ProvideMergeRequestRepository(testGORM(t))

	branchID := newBranch(t, "meta-lost-update")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "meta_lost_update_original")

	setKeyMeta(t, branchID, keyID, "meta_lost_update_branch_name", "active")

	// A concurrent writer renames the key on master AFTER the branch captured
	// its base — the write the unguarded UPDATE would have destroyed.
	_, err := testDB.Exec(`
		UPDATE keys SET name = 'meta_lost_update_concurrent', version = version + 1
		 WHERE id = $1`, keyID)
	require.NoError(t, err)

	applied, blocked, err := mrs.ApplyKeyMeta(ctx, nil, mrID, branchID, "merger@you.co")
	require.NoError(t, err)
	assert.Equal(t, 1, blocked, "the raced delta must be counted, so the merge can refuse")
	assert.Zero(t, applied)

	var name string
	require.NoError(t, testDB.QueryRow(`SELECT name FROM keys WHERE id = $1`, keyID).Scan(&name))
	assert.Equal(t, "meta_lost_update_concurrent", name,
		"the concurrent master rename must survive — overwriting it silently is THE bug")

	// And no history row claims a merge that never applied.
	var mergeRows int
	require.NoError(t, testDB.QueryRow(`
		SELECT count(*) FROM key_history WHERE key_id = $1 AND source = 'merge'`,
		keyID).Scan(&mergeRows))
	assert.Zero(t, mergeRows)
}
