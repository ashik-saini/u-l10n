package integrationtests

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
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

// applyDeltas mirrors applyDeltasSQL, including the version guard and the
// blocked/applied counts. `blocked` is the lost-update tripwire: an eligible
// delta whose master row moved past base_master_version with no 'mine'
// decision, which the real merge turns into ErrConcurrentMasterWrite and a
// full rollback.
func applyDeltas(t *testing.T, branchID, mrID int64) (blocked, applied int64) {
	t.Helper()
	err := testDB.QueryRow(`
		WITH eligible AS (
		    SELECT bt.key_id, bt.locale_id, bt.value, bt.render_hint,
		           (COALESCE(t.version, 0) = bt.base_master_version
		            OR COALESCE(mcr.resolution, '') = 'mine')       AS applicable
		      FROM branch_translations bt
		      LEFT JOIN translations t
		        ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
		      LEFT JOIN merge_conflict_resolutions mcr
		        ON mcr.merge_request_id = $2 AND mcr.key_id = bt.key_id
		       AND COALESCE(mcr.locale_id, -1) = bt.locale_id
		     WHERE bt.branch_id = $1 AND NOT bt.is_removed
		       AND NOT (COALESCE(mcr.resolution, '') = 'master'
		                AND COALESCE(t.version, 0) <> bt.base_master_version)
		), applied AS (
		    INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
		    SELECT e.key_id, e.locale_id, e.value, e.render_hint, 1, 'merger@you.co', now()
		      FROM eligible e
		     WHERE e.applicable
		    ON CONFLICT (key_id, locale_id) DO UPDATE SET
		        value = EXCLUDED.value, version = translations.version + 1, updated_at = now()
		    RETURNING key_id, locale_id, value, render_hint, version
		), recorded AS (
		    INSERT INTO translation_history
		        (key_id, locale_id, value, render_hint, version, source, branch_id, changed_by)
		    SELECT key_id, locale_id, value, render_hint, version, 'merge', $1, 'merger@you.co'
		      FROM applied
		)
		SELECT count(*) FILTER (WHERE NOT e.applicable) AS blocked,
		       (SELECT count(*) FROM applied)           AS applied
		  FROM eligible e`,
		branchID, mrID).Scan(&blocked, &applied)
	require.NoError(t, err)
	return blocked, applied
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
	blocked, applied := applyDeltas(t, branchID, mrID)
	require.Zero(t, blocked, "a clean delta trips no guard")
	require.Equal(t, int64(1), applied)

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

	// Master moves under BOTH pairs, making the conflicts real — a resolution
	// only exists for a conflict, and the apply predicate honours a 'master'
	// skip only while its conflict still stands.
	for _, k := range []int64{keepMine, keepMaster} {
		_, err := testDB.Exec(`
			UPDATE translations SET value = 'master moved', version = version + 1
			 WHERE key_id = $1 AND locale_id = $2`, k, enSG)
		require.NoError(t, err)
	}

	// The reviewer keeps the branch's value for one, master's for the other.
	for k, res := range map[int64]string{keepMine: "mine", keepMaster: "master"} {
		_, err := testDB.Exec(`
			INSERT INTO merge_conflict_resolutions
			    (merge_request_id, key_id, locale_id, resolution, resolved_by)
			VALUES ($1, $2, $3, $4, 'approver@you.co')`, mrID, k, enSG, res)
		require.NoError(t, err)
	}

	blocked, applied := applyDeltas(t, branchID, mrID)
	assert.Zero(t, blocked, "every conflict carries a decision, so nothing is blocked")
	assert.Equal(t, int64(1), applied, "only the 'mine' side lands")

	mine, _ := masterValue(t, keepMine, enSG)
	assert.Equal(t, "branch value", mine, "resolution 'mine' applies the branch value")

	master, _ := masterValue(t, keepMaster, enSG)
	assert.Equal(t, "master moved", master,
		"resolution 'master' must DISCARD the branch value, not apply it")
}

// TestMergeApplyRefusesAConcurrentMasterWrite pins the lost-update guard at the
// SQL level: a delta whose master row moved past base_master_version, with NO
// resolution row deciding it, must be counted as blocked and NOT applied.
//
// Through the full merge this state is unreachable deterministically — the
// conflict check refuses first, and only a write racing in between the check
// and the apply can produce it — which is exactly why the apply statement must
// carry the guard itself. mergesvc turns a non-zero blocked count into
// ErrConcurrentMasterWrite and the whole transaction rolls back.
func TestMergeApplyRefusesAConcurrentMasterWrite(t *testing.T) {
	branchID := newBranch(t, "merge-lost-update")
	mrID := newMR(t, branchID, true)
	enSG := localeID(t, "en-SG")

	raced := insertKey(t, "merge_lost_update_raced")
	clean := insertKey(t, "merge_lost_update_clean")

	for _, k := range []int64{raced, clean} {
		_, err := testDB.Exec(`
			INSERT INTO translations (key_id, locale_id, value, updated_by)
			VALUES ($1, $2, 'master v1', 'test@you.co')`, k, enSG)
		require.NoError(t, err)
	}

	setValue(t, branchID, raced, enSG, "branch value")
	setValue(t, branchID, clean, enSG, "branch value")

	// A concurrent writer lands on master AFTER the branch captured its base —
	// the write the old statement would have silently destroyed.
	_, err := testDB.Exec(`
		UPDATE translations SET value = 'concurrent write', version = version + 1
		 WHERE key_id = $1 AND locale_id = $2`, raced, enSG)
	require.NoError(t, err)

	blocked, applied := applyDeltas(t, branchID, mrID)
	assert.Equal(t, int64(1), blocked, "the raced delta must be counted, so the merge can refuse")
	assert.Equal(t, int64(1), applied, "the clean delta still matches its base")

	value, _ := masterValue(t, raced, enSG)
	assert.Equal(t, "concurrent write", value,
		"the concurrent master write must survive — overwriting it silently is THE bug")

}

// TestSpuriousMasterResolutionDoesNotDiscardACleanDelta.
//
// The 'master' skip is honoured only while the conflict it decided still
// stands. A resolution row recorded against a pair whose versions agree is
// spurious — honouring it would silently throw away a delta nobody disputed,
// which is the SQL-level half of the Resolve validation fix.
func TestSpuriousMasterResolutionDoesNotDiscardACleanDelta(t *testing.T) {
	branchID := newBranch(t, "merge-spurious-master")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "merge_spurious_master")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'master v1', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	setValue(t, branchID, keyID, enSG, "clean branch value")

	// The stray row: nothing conflicts, yet somebody recorded 'master'.
	_, err = testDB.Exec(`
		INSERT INTO merge_conflict_resolutions
		    (merge_request_id, key_id, locale_id, resolution, resolved_by)
		VALUES ($1, $2, $3, 'master', 'approver@you.co')`, mrID, keyID, enSG)
	require.NoError(t, err)

	blocked, applied := applyDeltas(t, branchID, mrID)
	assert.Zero(t, blocked)
	assert.Equal(t, int64(1), applied,
		"a clean delta with a stray 'master' row must still land")

	value, _ := masterValue(t, keyID, enSG)
	assert.Equal(t, "clean branch value", value)
}

// TestMergeApplyRecordsHistory: the History endpoint must not lie after a
// merge — every value a merge lands leaves a translation_history row with
// source 'merge', exactly as a UI write leaves one with source 'ui'.
func TestMergeApplyRecordsHistory(t *testing.T) {
	branchID := newBranch(t, "merge-history")
	mrID := newMR(t, branchID, true)
	keyID := insertKey(t, "merge_history_case")
	enSG := localeID(t, "en-SG")

	setValue(t, branchID, keyID, enSG, "merged copy")
	_, applied := applyDeltas(t, branchID, mrID)
	require.Equal(t, int64(1), applied)

	var (
		value   string
		source  string
		version int
		branch  int64
	)
	require.NoError(t, testDB.QueryRow(`
		SELECT value, source, version, branch_id FROM translation_history
		 WHERE key_id = $1 AND locale_id = $2
		 ORDER BY id DESC LIMIT 1`, keyID, enSG).Scan(&value, &source, &version, &branch))
	assert.Equal(t, "merged copy", value)
	assert.Equal(t, "merge", source)
	assert.Equal(t, 1, version, "the post-apply version, same convention as a UI write")
	assert.Equal(t, branchID, branch, "anchored on the branch that carried the change")
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

	// Mirrors removeDeltasSQL: the same eligibility and version guard as the
	// value apply, with the tombstone's history row carrying a NULL value.
	var blocked, removed int64
	require.NoError(t, testDB.QueryRow(`
		WITH eligible AS (
		    SELECT bt.key_id, bt.locale_id,
		           (COALESCE(t.version, 0) = bt.base_master_version
		            OR COALESCE(mcr.resolution, '') = 'mine')       AS applicable
		      FROM branch_translations bt
		      LEFT JOIN translations t
		        ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
		      LEFT JOIN merge_conflict_resolutions mcr
		        ON mcr.merge_request_id = $2 AND mcr.key_id = bt.key_id
		       AND COALESCE(mcr.locale_id, -1) = bt.locale_id
		     WHERE bt.branch_id = $1 AND bt.is_removed
		       AND NOT (COALESCE(mcr.resolution, '') = 'master'
		                AND COALESCE(t.version, 0) <> bt.base_master_version)
		), removed AS (
		    DELETE FROM translations t
		     USING eligible e
		     WHERE e.applicable
		       AND t.key_id = e.key_id AND t.locale_id = e.locale_id
		    RETURNING t.key_id, t.locale_id, t.render_hint, t.version
		), recorded AS (
		    INSERT INTO translation_history
		        (key_id, locale_id, value, render_hint, version, source, branch_id, changed_by)
		    SELECT key_id, locale_id, NULL, render_hint, version, 'merge', $1, 'merger@you.co'
		      FROM removed
		)
		SELECT count(*) FILTER (WHERE NOT e.applicable) AS blocked,
		       (SELECT count(*) FROM removed)           AS removed
		  FROM eligible e`, branchID, mrID).Scan(&blocked, &removed))
	assert.Zero(t, blocked)
	assert.Equal(t, int64(1), removed)

	_, found := masterValue(t, keyID, enSG)
	assert.False(t, found,
		"a removal DELETES the row (untranslated); blanking it to \"\" would be a different fact")

	// And the history row says "became untranslated": a NULL value, never "".
	var histValue *string
	require.NoError(t, testDB.QueryRow(`
		SELECT value FROM translation_history
		 WHERE key_id = $1 AND locale_id = $2 AND source = 'merge'
		 ORDER BY id DESC LIMIT 1`, keyID, enSG).Scan(&histValue))
	assert.Nil(t, histValue, "a merge removal is recorded as NULL, not as an empty string")
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

		// Hashed in SQL over the jsonb text form, exactly as
		// materialiseBundleSQL does — the test mirrors production. Key order
		// in the INPUT deliberately varies below; jsonb canonicalises it.
		var sha string
		require.NoError(t, testDB.QueryRow(`
			INSERT INTO release_bundles (release_id, locale_id, strings, sha256, key_count, byte_size)
			VALUES ($1, $2, $3::jsonb,
			        encode(sha256(convert_to(($3::jsonb)::text, 'UTF8')), 'hex'),
			        1,
			        octet_length(($3::jsonb)::text))
			RETURNING sha256`, relID, enSG, payload).Scan(&sha))

		return sha
	}

	a := shaFor(`{"k":"v","z":"end"}`)
	b := shaFor(`{"z":"end","k":"v"}`)
	c := shaFor(`{"k":"different","z":"end"}`)

	assert.Equal(t, a, b,
		"identical content must produce an identical ETag, whatever the input's key order")
	assert.NotEqual(t, a, c, "changed content must produce a different ETag")
}

// TestBundleShaDescribesTheServedBytes is the client-checksum contract: the
// stored sha256 and byte_size must describe the EXACT bytes the OTA path
// serves — `strings::text`, Postgres's re-serialisation of the jsonb column —
// not the Go json.Marshal bytes that went in, whose key order and spacing
// differ. ETag/304 only needs a stable fingerprint; a client verifying its
// download against the advertised sha needs the fingerprint to be OF the
// download.
func TestBundleShaDescribesTheServedBytes(t *testing.T) {
	ctx := context.Background()
	releases := repository.ProvideReleaseRepository(testGORM(t))
	enSG := localeID(t, "en-SG")

	rel, err := releases.Create(ctx, nil, "publish", nil, "test@you.co")
	require.NoError(t, err)
	// Quarantine the release afterwards so the OTA tests' view of "newest
	// eligible" is undisturbed — same discipline as mergeAndQuarantine.
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`
			UPDATE releases SET rolled_back_at = now(), rolled_back_by = 'test-cleanup'
			 WHERE id = $1 AND rolled_back_at IS NULL`, rel.ID)
		require.NoError(t, cleanupErr)
	})

	require.NoError(t, releases.MaterialiseBundle(ctx, nil, rel.ID,
		model.Locale{ID: enSG, Code: "en-SG"}, []repository.ExportRow{
			{Key: "bundle_sha_first", Value: "exactly as served", Found: true},
			{Key: "bundle_sha_second", Value: "", Found: true},
		}))

	served, err := releases.ServableBundle(ctx, nil, enSG, "")
	require.NoError(t, err)
	require.Equal(t, rel.Version, served.ReleaseVersion,
		"the just-cut release is the newest eligible one")

	sum := sha256.Sum256(served.Strings)
	assert.Equal(t, hex.EncodeToString(sum[:]), served.SHA256,
		"sha256(served bytes) must equal the stored fingerprint")

	stored, err := releases.BundleFor(ctx, nil, rel.Version, enSG)
	require.NoError(t, err)
	assert.Equal(t, served.SHA256, stored.SHA256)
	assert.Equal(t, len(served.Strings), stored.ByteSize,
		"byte_size must count the served bytes, not the Go-marshalled ones")
}

// TestPublishVersionRaceIsRetryable forces the 23505 the CreatePublish comment
// promises: two transactions read the same max(version) and the loser must
// surface as the retryable ErrReleaseVersionRace, never as an unclassified
// driver error that becomes a 500.
func TestPublishVersionRaceIsRetryable(t *testing.T) {
	ctx := context.Background()
	conn := testGORM(t)
	releases := repository.ProvideReleaseRepository(conn)

	tx1 := conn.GetDB().Begin()
	require.NoError(t, tx1.Error)
	_, err := releases.CreatePublish(ctx, tx1, "first", nil, "a@you.co")
	require.NoError(t, err)

	// The second publish computes the same max+1 — tx1's row is uncommitted
	// and invisible — then blocks on the unique index until tx1 commits, at
	// which point it must lose with a classified, retryable error.
	raced := make(chan error, 1)
	go func() {
		tx2 := conn.GetDB().Begin()
		if tx2.Error != nil {
			raced <- tx2.Error
			return
		}
		_, err := releases.CreatePublish(ctx, tx2, "second", nil, "b@you.co")
		tx2.Rollback()
		raced <- err
	}()

	// Give tx2 time to reach the index wait before releasing it. If it has
	// not started yet the test still cannot pass vacuously — a late tx2 sees
	// the committed row, allocates max+2 and returns nil, failing the
	// assertion below rather than flaking silently green.
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, tx1.Commit().Error)

	err = <-raced
	require.Error(t, err, "the losing publish must not silently take a new number")
	assert.ErrorIs(t, err, repository.ErrReleaseVersionRace,
		"the 23505 must be classified as the retryable race, not passed through raw")
}
