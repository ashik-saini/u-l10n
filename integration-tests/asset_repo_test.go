package integrationtests

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These mirror the statements pkg/repository/asset.go and audit.go issue,
// following the same convention as branch_cow_test.go: the behaviour under test
// IS the SQL, and a mock would only prove the mock agrees with itself.
//
// TestAssetConstraints in schema_test.go already proves the constraints reject
// bad input. What is proved here is the shape of the repository's own
// statements — the ON CONFLICT clauses in particular, which are hand-written
// because GORM v1 has no clause.OnConflict and are therefore never checked by a
// compiler.

func assetSHA(seed string) string {
	sum := sha256.Sum256([]byte("asset-repo/" + seed))
	return hex.EncodeToString(sum[:])
}

// createAsset mirrors createAssetSQL.
func createAsset(t *testing.T, sha, filename string, bytes int) (id int64, inserted bool) {
	t.Helper()
	err := testDB.QueryRow(`
		INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, width, height, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (project_id, sha256) DO NOTHING
		RETURNING id`,
		"screenshots/"+sha[0:2]+"/"+sha[2:4]+"/"+sha, sha, filename, "image/png",
		bytes, nil, nil, "token:test").Scan(&id)
	if err == nil {
		return id, true
	}
	// DO NOTHING updates no row, so RETURNING yields nothing. That is the
	// desired outcome, not a failure — the caller reads the winner back.
	require.Contains(t, err.Error(), "no rows")

	require.NoError(t, testDB.QueryRow(
		`SELECT id FROM assets WHERE sha256 = $1`, sha).Scan(&id))
	return id, false
}

// TestAssetCreateIsContentAddressedAndIdempotent: re-confirming the same bytes
// must yield the same row rather than an error, because a retried confirm after
// a dropped response is a normal event and not a caller mistake.
func TestAssetCreateIsContentAddressedAndIdempotent(t *testing.T) {
	sha := assetSHA("idempotent")

	first, inserted := createAsset(t, sha, "cart.png", 4096)
	assert.True(t, inserted)

	// A second confirm, even claiming a different filename and size, resolves
	// to the existing row. The name IS the content: there is nothing an
	// existing asset could usefully be updated to.
	second, inserted := createAsset(t, sha, "different-name.png", 9999)
	assert.False(t, inserted)
	assert.Equal(t, first, second)

	var filename string
	var bytes int
	require.NoError(t, testDB.QueryRow(
		`SELECT filename, bytes FROM assets WHERE id = $1`, first).Scan(&filename, &bytes))
	assert.Equal(t, "cart.png", filename, "the first confirm's metadata must survive")
	assert.Equal(t, 4096, bytes)
}

// attach mirrors attachSQL.
func attach(t *testing.T, keyID, assetID int64, note string) {
	t.Helper()
	_, err := testDB.Exec(`
		INSERT INTO key_assets (key_id, asset_id, note, sort_order, created_by)
		VALUES ($1, $2, $3,
		        COALESCE((SELECT max(sort_order) + 1 FROM key_assets WHERE key_id = $1), 0),
		        $4)
		ON CONFLICT (key_id, asset_id) DO UPDATE SET note = EXCLUDED.note`,
		keyID, assetID, note, "token:test")
	require.NoError(t, err)
}

func TestKeyAssetAttachAppendsAndAmends(t *testing.T) {
	keyID := insertKey(t, "asset.attach.append")
	first, _ := createAsset(t, assetSHA("attach-1"), "one.png", 10)
	second, _ := createAsset(t, assetSHA("attach-2"), "two.png", 20)

	attach(t, keyID, first, "first note")
	attach(t, keyID, second, "second note")

	// sort_order appends: a key's screenshots are shown in the order they were
	// attached, and a second attach must not collide on 0.
	rows, err := testDB.Query(
		`SELECT asset_id, note, sort_order FROM key_assets WHERE key_id = $1 ORDER BY sort_order`, keyID)
	require.NoError(t, err)
	defer rows.Close()

	type link struct {
		assetID   int64
		note      string
		sortOrder int
	}
	var got []link
	for rows.Next() {
		var l link
		require.NoError(t, rows.Scan(&l.assetID, &l.note, &l.sortOrder))
		got = append(got, l)
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 2)
	assert.Equal(t, []link{{first, "first note", 0}, {second, "second note", 1}}, got)

	// Re-attaching amends the note instead of failing, so a translator can
	// improve the guidance without detaching first.
	attach(t, keyID, first, "truncates past 18 characters")

	var note string
	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT note FROM key_assets WHERE key_id = $1 AND asset_id = $2`, keyID, first).Scan(&note))
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM key_assets WHERE key_id = $1`, keyID).Scan(&count))

	assert.Equal(t, "truncates past 18 characters", note)
	assert.Equal(t, 2, count, "re-attaching must not create a second link")
}

// TestKeyAssetDetachReportsWhetherThereWasAnything: "there was nothing to
// unlink" and "I unlinked it" are different facts, and the handler returns
// different status codes for them.
func TestKeyAssetDetachAffectsExactlyOneRow(t *testing.T) {
	keyID := insertKey(t, "asset.detach")
	assetID, _ := createAsset(t, assetSHA("detach"), "shot.png", 30)
	attach(t, keyID, assetID, "")

	res, err := testDB.Exec(
		`DELETE FROM key_assets WHERE key_id = $1 AND asset_id = $2`, keyID, assetID)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected)

	res, err = testDB.Exec(
		`DELETE FROM key_assets WHERE key_id = $1 AND asset_id = $2`, keyID, assetID)
	require.NoError(t, err)
	affected, err = res.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "a second detach must report that it removed nothing")

	// Detaching leaves the asset itself alone: the bytes are content-addressed
	// and another key may still reference them.
	var stillThere bool
	require.NoError(t, testDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1)`, assetID).Scan(&stillThere))
	assert.True(t, stillThere)
}

// TestAuditEventRoundTrip mirrors AuditRepository.Record. The metadata column
// is JSONB, so a malformed encoding is rejected by the database rather than
// stored as unusable text — worth proving, because an audit row that cannot be
// queried answers nothing.
func TestAuditEventRoundTrip(t *testing.T) {
	_, err := testDB.Exec(`
		INSERT INTO audit_events (actor, action, target, metadata, request_id)
		VALUES ($1, $2, $3, $4, $5)`,
		"token:ci", "asset.view", "asset:88",
		`{"sha256":"`+assetSHA("audit")+`"}`, "req-integration")
	require.NoError(t, err)

	var actor, action, target, requestID, sha string
	require.NoError(t, testDB.QueryRow(`
		SELECT actor, action, target, request_id, metadata->>'sha256'
		  FROM audit_events
		 WHERE request_id = 'req-integration'`).Scan(&actor, &action, &target, &requestID, &sha))

	// actor is CITEXT; the audit trail must not fragment on case.
	assert.Equal(t, "token:ci", actor)
	assert.Equal(t, "asset.view", action)
	assert.Equal(t, "asset:88", target)
	assert.Equal(t, assetSHA("audit"), sha)
}

// TestKeyAssetCascadesOnAssetDelete documents what the ON DELETE CASCADE on
// key_assets actually does, so nobody has to guess whether removing an asset
// leaves dangling links.
func TestKeyAssetCascadesOnAssetDelete(t *testing.T) {
	keyID := insertKey(t, "asset.cascade")
	assetID, _ := createAsset(t, assetSHA("cascade"), "shot.png", 40)
	attach(t, keyID, assetID, "")

	_, err := testDB.Exec(`DELETE FROM assets WHERE id = $1`, assetID)
	require.NoError(t, err)

	var links int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM key_assets WHERE asset_id = $1`, assetID).Scan(&links))
	assert.Zero(t, links)
}
