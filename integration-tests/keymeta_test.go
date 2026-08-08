package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		ON CONFLICT (branch_id, key_id) WHERE key_id IS NOT NULL DO UPDATE SET
		    name = EXCLUDED.name, status = EXCLUDED.status, updated_at = now()`,
		branchID, keyID, name, status)
	require.NoError(t, err)
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
// and have that reach master through the metadata path.
func TestSoftDeleteOnBranchMergesAsAStatusChange(t *testing.T) {
	branchID := newBranch(t, "meta-softdelete")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "meta_softdelete_case")

	setKeyMeta(t, branchID, keyID, "meta_softdelete_case", "deleted")

	_, err := testDB.Exec(`
		UPDATE keys k
		   SET name = bk.name, platforms = bk.platforms, status = bk.status,
		       version = k.version + 1, updated_at = now()
		  FROM branch_keys bk
		  LEFT JOIN merge_conflict_resolutions mcr
		    ON mcr.merge_request_id = $2 AND mcr.key_id = bk.key_id AND mcr.locale_id IS NULL
		 WHERE bk.branch_id = $1 AND bk.key_id = k.id
		   AND COALESCE(mcr.resolution, 'mine') <> 'master'`, branchID, mrID)
	require.NoError(t, err)

	var status string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM keys WHERE id = $1`, keyID).Scan(&status))
	assert.Equal(t, "deleted", status, "a branch soft delete must reach master")

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

	_, err = testDB.Exec(`
		UPDATE keys k SET name = bk.name, version = k.version + 1
		  FROM branch_keys bk
		  LEFT JOIN merge_conflict_resolutions mcr
		    ON mcr.merge_request_id = $2 AND mcr.key_id = bk.key_id AND mcr.locale_id IS NULL
		 WHERE bk.branch_id = $1 AND bk.key_id = k.id
		   AND COALESCE(mcr.resolution, 'mine') <> 'master'`, branchID, mrID)
	require.NoError(t, err)

	var name string
	require.NoError(t, testDB.QueryRow(`SELECT name FROM keys WHERE id = $1`, keyID).Scan(&name))
	assert.Equal(t, "meta_resolution_master_name", name,
		"resolution 'master' must DISCARD the branch rename, not apply it")
}
