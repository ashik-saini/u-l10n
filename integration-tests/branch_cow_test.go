package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These exercise the copy-on-write semantics directly in SQL, mirroring what
// pkg/repository/branch.go issues. They live here rather than as unit tests
// because the behaviour under test IS the SQL — a mock would only prove the
// mock agrees with itself.

func newBranch(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (name, created_by) VALUES ($1, 'test@you.co') RETURNING id`,
		name).Scan(&id))
	return id
}

// resolve mirrors resolveSQL: delta if present, else master.
func resolve(t *testing.T, branchID, keyID int64, localeID int16) (value string, found, fromDelta bool, masterVersion int) {
	t.Helper()
	err := testDB.QueryRow(`
		SELECT COALESCE(bt.value, t.value, ''),
		       CASE WHEN bt.key_id IS NOT NULL THEN NOT bt.is_removed
		            ELSE t.key_id IS NOT NULL END,
		       (bt.key_id IS NOT NULL),
		       COALESCE(t.version, 0)
		  FROM (SELECT $2::bigint AS key_id, $3::smallint AS locale_id) AS want
		  LEFT JOIN translations t
		    ON t.key_id = want.key_id AND t.locale_id = want.locale_id
		  LEFT JOIN branch_translations bt
		    ON bt.key_id = want.key_id AND bt.locale_id = want.locale_id AND bt.branch_id = $1`,
		branchID, keyID, localeID).Scan(&value, &found, &fromDelta, &masterVersion)
	require.NoError(t, err)
	return
}

// setValue mirrors setValueSQL, including capturing base_master_version only on
// the first touch.
func setValue(t *testing.T, branchID, keyID int64, localeID int16, value string) {
	t.Helper()
	_, err := testDB.Exec(`
		INSERT INTO branch_translations
		    (branch_id, key_id, locale_id, value, render_hint, is_removed, base_master_version, updated_by)
		VALUES ($1, $2, $3, $4, 'plain', FALSE,
		        COALESCE((SELECT version FROM translations WHERE key_id = $2 AND locale_id = $3), 0),
		        'test@you.co')
		ON CONFLICT (branch_id, key_id, locale_id) DO UPDATE SET
		    value = EXCLUDED.value, is_removed = FALSE, updated_at = now()`,
		branchID, keyID, localeID, value)
	require.NoError(t, err)
}

func baseVersion(t *testing.T, branchID, keyID int64, localeID int16) int {
	t.Helper()
	var v int
	require.NoError(t, testDB.QueryRow(`
		SELECT base_master_version FROM branch_translations
		 WHERE branch_id = $1 AND key_id = $2 AND locale_id = $3`,
		branchID, keyID, localeID).Scan(&v))
	return v
}

func TestCOWReadFallsThroughToMaster(t *testing.T) {
	branchID := newBranch(t, "cow-fallthrough")
	keyID := insertKey(t, "cow_fallthrough_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'from master', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	value, found, fromDelta, masterVersion := resolve(t, branchID, keyID, enSG)
	assert.Equal(t, "from master", value)
	assert.True(t, found)
	assert.False(t, fromDelta, "a branch with no delta must see master unchanged")
	assert.Equal(t, 1, masterVersion)

	setValue(t, branchID, keyID, enSG, "from branch")

	value, found, fromDelta, _ = resolve(t, branchID, keyID, enSG)
	assert.Equal(t, "from branch", value)
	assert.True(t, found)
	assert.True(t, fromDelta, "the delta must win once written")

	// Master is untouched — that is the whole point of copy-on-write.
	var master string
	require.NoError(t, testDB.QueryRow(
		`SELECT value FROM translations WHERE key_id = $1 AND locale_id = $2`,
		keyID, enSG).Scan(&master))
	assert.Equal(t, "from master", master, "editing a branch must not touch master")
}

// TestBaseMasterVersionCapturedOnFirstTouchOnly is the most important test in
// this file.
//
// base_master_version records where the branch STARTED, not what it has done
// since. If a later edit refreshed it, the branch would silently adopt master's
// newer version as its base and a genuine conflict would look clean — which is
// exactly the silent-overwrite the whole merge design exists to prevent.
func TestBaseMasterVersionCapturedOnFirstTouchOnly(t *testing.T) {
	branchID := newBranch(t, "cow-base-version")
	keyID := insertKey(t, "cow_base_version_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'v1', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	setValue(t, branchID, keyID, enSG, "branch edit 1")
	assert.Equal(t, 1, baseVersion(t, branchID, keyID, enSG), "captured master version at first touch")

	// Master moves on while the branch is open.
	_, err = testDB.Exec(`
		UPDATE translations SET value = 'v2', version = version + 1
		 WHERE key_id = $1 AND locale_id = $2`, keyID, enSG)
	require.NoError(t, err)

	setValue(t, branchID, keyID, enSG, "branch edit 2")

	assert.Equal(t, 1, baseVersion(t, branchID, keyID, enSG),
		"base_master_version must NOT be refreshed by a later edit; refreshing it would make a real conflict look clean")

	// And this is how the merge detects it: one integer comparison.
	var masterVersion int
	require.NoError(t, testDB.QueryRow(
		`SELECT version FROM translations WHERE key_id = $1 AND locale_id = $2`,
		keyID, enSG).Scan(&masterVersion))
	assert.NotEqual(t, baseVersion(t, branchID, keyID, enSG), masterVersion,
		"COALESCE(master.version,0) <> base_master_version -> CONFLICT")
}

// TestBaseVersionZeroMeansNoMasterRow covers the create/create race: a branch
// that creates a value where master had none records 0, so master gaining one
// later is detectable as a conflict by the same comparison.
func TestBaseVersionZeroMeansNoMasterRow(t *testing.T) {
	branchID := newBranch(t, "cow-base-zero")
	keyID := insertKey(t, "cow_base_zero_case")
	enSG := localeID(t, "en-SG")

	setValue(t, branchID, keyID, enSG, "created on branch")
	assert.Equal(t, 0, baseVersion(t, branchID, keyID, enSG),
		"0 records that no master row existed at first touch")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'someone else created it', 'other@you.co')`, keyID, enSG)
	require.NoError(t, err)

	var masterVersion int
	require.NoError(t, testDB.QueryRow(
		`SELECT version FROM translations WHERE key_id = $1 AND locale_id = $2`,
		keyID, enSG).Scan(&masterVersion))

	assert.NotEqual(t, 0, masterVersion, "create/create race is detected by the same comparison")
}

// TestTombstoneResolvesToUntranslated proves the third state survives on a
// branch: removing is not the same as setting an empty string.
func TestTombstoneResolvesToUntranslated(t *testing.T) {
	branchID := newBranch(t, "cow-tombstone")
	keyID := insertKey(t, "cow_tombstone_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'on master', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	_, err = testDB.Exec(`
		INSERT INTO branch_translations
		    (branch_id, key_id, locale_id, value, is_removed, base_master_version, updated_by)
		VALUES ($1, $2, $3, NULL, TRUE, 1, 'test@you.co')`, branchID, keyID, enSG)
	require.NoError(t, err)

	_, found, fromDelta, _ := resolve(t, branchID, keyID, enSG)
	assert.False(t, found, "a tombstone reads as UNTRANSLATED, not as an empty string")
	assert.True(t, fromDelta)

	// Contrast: an explicitly empty delta is found, with an empty value.
	other := insertKey(t, "cow_empty_delta_case")
	setValue(t, branchID, other, enSG, "")

	value, found, _, _ := resolve(t, branchID, other, enSG)
	assert.True(t, found, "an explicitly empty delta is PRESENT")
	assert.Equal(t, "", value)
}
