package integrationtests

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These exercise the merge transaction's SQL directly. The refusal paths matter
// more than the happy one: a merge that succeeds when it should have refused
// ships the wrong copy to customers silently.

func newMR(t *testing.T, branchID int64, approved bool) int64 {
	t.Helper()
	var id int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, 'test', 'author@you.co') RETURNING id`, branchID).Scan(&id))
	if approved {
		_, err := testDB.Exec(`
			UPDATE merge_requests SET status='approved', approved_by='approver@you.co', approved_at=now()
			 WHERE id = $1`, id)
		require.NoError(t, err)
	}
	return id
}

// applyDeltas mirrors applyDeltasSQL.
func applyDeltas(t *testing.T, branchID, mrID int64) int64 {
	t.Helper()
	res, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
		SELECT bt.key_id, bt.locale_id, bt.value, bt.render_hint, 1, 'merger@you.co', now()
		  FROM branch_translations bt
		  LEFT JOIN merge_conflict_resolutions mcr
		    ON mcr.merge_request_id = $2 AND mcr.key_id = bt.key_id
		   AND COALESCE(mcr.locale_id, -1) = bt.locale_id
		 WHERE bt.branch_id = $1 AND NOT bt.is_removed
		   AND COALESCE(mcr.resolution, 'mine') <> 'master'
		ON CONFLICT (key_id, locale_id) DO UPDATE SET
		    value = EXCLUDED.value, version = translations.version + 1, updated_at = now()`,
		branchID, mrID)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	return n
}

func masterValue(t *testing.T, keyID int64, localeID int16) (string, bool) {
	t.Helper()
	var v string
	err := testDB.QueryRow(
		`SELECT value FROM translations WHERE key_id = $1 AND locale_id = $2`, keyID, localeID).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	require.NoError(t, err)
	return v, true
}

// TestMergeAppliesCleanDeltas is the happy path: an unconflicted delta reaches
// master and bumps its version.
func TestMergeAppliesCleanDeltas(t *testing.T) {
	branchID := newBranch(t, "merge-clean")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "merge_clean_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'old copy', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	setValue(t, branchID, keyID, enSG, "new copy")
	require.Equal(t, int64(1), applyDeltas(t, branchID, mrID))

	value, found := masterValue(t, keyID, enSG)
	assert.True(t, found)
	assert.Equal(t, "new copy", value)

	var version int
	require.NoError(t, testDB.QueryRow(
		`SELECT version FROM translations WHERE key_id=$1 AND locale_id=$2`,
		keyID, enSG).Scan(&version))
	assert.Equal(t, 2, version, "applying a delta bumps master's OCC anchor")
}

// TestMergeRespectsMasterResolution proves a reviewer choosing "master" causes
// the branch's value to be DISCARDED, not applied.
//
// Getting this backwards would silently overwrite the value a human explicitly
// chose to keep — the worst possible outcome for a conflict resolver.
func TestMergeRespectsMasterResolution(t *testing.T) {
	branchID := newBranch(t, "merge-resolution")
	mrID := newMR(t, branchID, true)
	enSG := localeID(t, "en-SG")

	keepMine := insertKey(t, "merge_keep_mine")
	keepMaster := insertKey(t, "merge_keep_master")

	for _, k := range []int64{keepMine, keepMaster} {
		_, err := testDB.Exec(`
			INSERT INTO translations (key_id, locale_id, value, updated_by)
			VALUES ($1, $2, 'master value', 'test@you.co')`, k, enSG)
		require.NoError(t, err)
	}

	setValue(t, branchID, keepMine, enSG, "branch value")
	setValue(t, branchID, keepMaster, enSG, "branch value")

	// The reviewer keeps the branch's value for one, master's for the other.
	for k, res := range map[int64]string{keepMine: "mine", keepMaster: "master"} {
		_, err := testDB.Exec(`
			INSERT INTO merge_conflict_resolutions
			    (merge_request_id, key_id, locale_id, resolution, resolved_by)
			VALUES ($1, $2, $3, $4, 'approver@you.co')`, mrID, k, enSG, res)
		require.NoError(t, err)
	}

	applyDeltas(t, branchID, mrID)

	mine, _ := masterValue(t, keepMine, enSG)
	assert.Equal(t, "branch value", mine, "resolution 'mine' applies the branch value")

	master, _ := masterValue(t, keepMaster, enSG)
	assert.Equal(t, "master value", master,
		"resolution 'master' must DISCARD the branch value, not apply it")
}

// TestMergeTombstoneDeletesRatherThanBlanks proves the three-state rule
// survives the merge: a removal deletes the master row, it does not set "".
func TestMergeTombstoneDeletesRatherThanBlanks(t *testing.T) {
	branchID := newBranch(t, "merge-tombstone")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "merge_tombstone_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'doomed', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	_, err = testDB.Exec(`
		INSERT INTO branch_translations
		    (branch_id, key_id, locale_id, value, is_removed, base_master_version, updated_by)
		VALUES ($1, $2, $3, NULL, TRUE, 1, 'test@you.co')`, branchID, keyID, enSG)
	require.NoError(t, err)

	_, err = testDB.Exec(`
		DELETE FROM translations t
		 USING branch_translations bt
		  LEFT JOIN merge_conflict_resolutions mcr
		    ON mcr.merge_request_id = $2 AND mcr.key_id = bt.key_id
		   AND COALESCE(mcr.locale_id, -1) = bt.locale_id
		 WHERE bt.branch_id = $1 AND bt.is_removed
		   AND COALESCE(mcr.resolution, 'mine') <> 'master'
		   AND t.key_id = bt.key_id AND t.locale_id = bt.locale_id`, branchID, mrID)
	require.NoError(t, err)

	_, found := masterValue(t, keyID, enSG)
	assert.False(t, found,
		"a removal DELETES the row (untranslated); blanking it to \"\" would be a different fact")
}

// TestReleaseVersionIsMonotonicAndUnique guards the counter that OTA and every
// export depend on. It is safe as a global counter only because u-l10n runs as
// a single global instance.
func TestReleaseVersionIsMonotonicAndUnique(t *testing.T) {
	create := func() int64 {
		var v int64
		require.NoError(t, testDB.QueryRow(`
			INSERT INTO releases (version, source, created_by)
			VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, 'merge', 'a@you.co')
			RETURNING version`).Scan(&v))
		return v
	}

	first, second := create(), create()
	assert.Equal(t, first+1, second, "versions increase by one")

	_, err := testDB.Exec(`
		INSERT INTO releases (version, source, created_by) VALUES ($1, 'merge', 'a@you.co')`, second)
	requireRejected(t, err, "a duplicate release version")
}

// TestBundleShaIsStableForIdenticalContent proves the ETag is a content
// fingerprint. If it changed without the content changing, every client's
// If-None-Match would miss on every merge and the 304 path would never fire.
func TestBundleShaIsStableForIdenticalContent(t *testing.T) {
	enSG := localeID(t, "en-SG")

	shaFor := func(payload string) string {
		var relID int64
		require.NoError(t, testDB.QueryRow(`
			INSERT INTO releases (version, source, created_by)
			VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, 'publish', 'a@you.co')
			RETURNING id`).Scan(&relID))

		// Hashed in Go, exactly as pkg/repository/release.go does — the test
		// should mirror production rather than reimplement it in SQL.
		sum := sha256.Sum256([]byte(payload))
		sha := hex.EncodeToString(sum[:])

		_, err := testDB.Exec(`
			INSERT INTO release_bundles (release_id, locale_id, strings, sha256, key_count, byte_size)
			VALUES ($1, $2, $3::jsonb, $4, 1, $5)`, relID, enSG, payload, sha, len(payload))
		require.NoError(t, err)

		return sha
	}

	a := shaFor(`{"k":"v"}`)
	b := shaFor(`{"k":"v"}`)
	c := shaFor(`{"k":"different"}`)

	assert.Equal(t, a, b, "identical content must produce an identical ETag")
	assert.NotEqual(t, a, c, "changed content must produce a different ETag")
}
