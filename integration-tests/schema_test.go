package integrationtests

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLocalesSeed pins the export directory names against u-mobile's run.sh.
//
// These are not cosmetic. The export zip must reproduce Lokalise's exact
// internal layout because run.sh has 22 `cp -rf` mappings keyed on these
// paths; a wrong directory name silently writes translations to a path nobody
// copies from. Note en-SG's Android directory is bare `values`, not
// `values-en-rSG`.
func TestLocalesSeed(t *testing.T) {
	want := []struct {
		code, flutter, android, ios string
	}{
		{"en-SG", "en_SG", "values", "en-SG.lproj"},
		{"en-MY", "en_MY", "values-en-rMY", "en-MY.lproj"},
		{"en-TH", "en_TH", "values-en-rTH", "en-TH.lproj"},
		{"th-TH", "th_TH", "values-th-rTH", "th-TH.lproj"},
		{"ms-MY", "ms_MY", "values-ms-rMY", "ms-MY.lproj"},
		{"en-AU", "en_AU", "values-en-rAU", "en-AU.lproj"},
	}

	var count int
	require.NoError(t, testDB.QueryRow(`SELECT count(*) FROM locales`).Scan(&count))
	assert.Equal(t, len(want), count, "exactly six locales must be seeded")

	for _, w := range want {
		t.Run(w.code, func(t *testing.T) {
			var flutter, android, ios string
			err := testDB.QueryRow(`
				SELECT flutter_dir, android_values_dir, ios_lproj
				FROM locales WHERE code = $1`, w.code).Scan(&flutter, &android, &ios)
			require.NoError(t, err)
			assert.Equal(t, w.flutter, flutter)
			assert.Equal(t, w.android, android)
			assert.Equal(t, w.ios, ios)
		})
	}
}

// TestKeyNameUniqueAmongActiveOnly proves the partial unique index does both
// halves of its job: it blocks a duplicate live name, and it does not turn a
// soft-deleted name into a permanent tombstone.
func TestKeyNameUniqueAmongActiveOnly(t *testing.T) {
	insertKey(t, "duplicate_name_case")

	_, err := testDB.Exec(`
		INSERT INTO keys (name, platforms, sort_index)
		VALUES ('duplicate_name_case', ARRAY['flutter']::TEXT[], $1)`, nextSortIndex())
	requireRejected(t, err, "idx_keys_name_active",
		"a second ACTIVE key with an existing name")

	_, err = testDB.Exec(`UPDATE keys SET status = 'deleted' WHERE name = 'duplicate_name_case'`)
	require.NoError(t, err)

	_, err = testDB.Exec(`
		INSERT INTO keys (name, platforms, sort_index)
		VALUES ('duplicate_name_case', ARRAY['flutter']::TEXT[], $1)`, nextSortIndex())
	assert.NoError(t, err, "a soft-deleted name must be reusable, not blocked forever")
}

func TestKeyConstraintsRejectBadInput(t *testing.T) {
	// constraint names the CHECK each case must trip. keys_platforms_check
	// covers both halves of its own predicate — the vocabulary and the
	// non-empty requirement — which is why two cases name it.
	cases := []struct {
		name, what, constraint, stmt string
	}{
		{
			name:       "unknown status",
			what:       "status 'archived'",
			constraint: "keys_status_check",
			stmt: `INSERT INTO keys (name, platforms, sort_index, status)
			       VALUES ('bad_status', ARRAY['flutter']::TEXT[], 900001, 'archived')`,
		},
		{
			name:       "unknown platform",
			what:       "platform 'windows'",
			constraint: "keys_platforms_check",
			stmt: `INSERT INTO keys (name, platforms, sort_index)
			       VALUES ('bad_platform', ARRAY['windows']::TEXT[], 900002)`,
		},
		{
			name:       "empty platform list",
			what:       "a key belonging to no platform",
			constraint: "keys_platforms_check",
			stmt: `INSERT INTO keys (name, platforms, sort_index)
			       VALUES ('no_platform', ARRAY[]::TEXT[], 900003)`,
		},
		{
			name:       "non-positive version",
			what:       "version 0",
			constraint: "keys_version_check",
			stmt: `INSERT INTO keys (name, platforms, sort_index, version)
			       VALUES ('bad_version', ARRAY['flutter']::TEXT[], 900004, 0)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testDB.Exec(tc.stmt)
			requireRejected(t, err, tc.constraint, tc.what)
		})
	}
}

// TestLokaliseKeyIDIsAnIdempotencyKey proves the importer can be re-run safely:
// the unique constraint is what makes ON CONFLICT DO UPDATE possible.
func TestLokaliseKeyIDIsAnIdempotencyKey(t *testing.T) {
	_, err := testDB.Exec(`
		INSERT INTO keys (name, platforms, sort_index, lokalise_key_id)
		VALUES ('imported_one', ARRAY['flutter']::TEXT[], $1, 55501)`, nextSortIndex())
	require.NoError(t, err)

	_, err = testDB.Exec(`
		INSERT INTO keys (name, platforms, sort_index, lokalise_key_id)
		VALUES ('imported_two', ARRAY['flutter']::TEXT[], $1, 55501)`, nextSortIndex())
	requireRejected(t, err, "keys_project_lokalise_key_unique",
		"a second key claiming the same lokalise_key_id")
}

// TestTranslationThreeStates is the most important test in this file.
//
// A (key, locale) pair has three distinguishable states. Collapsing any two
// corrupts production copy: absent->empty adds ~430 spurious keys to en-SG,
// empty->absent deletes 3,664 intentional blanks from ms-MY.
func TestTranslationThreeStates(t *testing.T) {
	keyID := insertKey(t, "three_state_case")
	enSG := localeID(t, "en-SG")
	enMY := localeID(t, "en-MY")
	msMY := localeID(t, "ms-MY")

	// en-SG: translated. en-MY: explicitly empty. ms-MY: no row at all.
	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'Log in', 'test@you.co'),
		       ($1, $3, '',       'test@you.co')`, keyID, enSG, enMY)
	require.NoError(t, err)

	read := func(locale int16) (value sql.NullString, found bool) {
		var v sql.NullString
		err := testDB.QueryRow(`
			SELECT value FROM translations WHERE key_id = $1 AND locale_id = $2`,
			keyID, locale).Scan(&v)
		if err == sql.ErrNoRows {
			return v, false
		}
		require.NoError(t, err)
		return v, true
	}

	v, found := read(enSG)
	assert.True(t, found)
	assert.Equal(t, "Log in", v.String)

	v, found = read(enMY)
	assert.True(t, found, "explicitly empty must be a ROW, exported as \"\"")
	assert.Equal(t, "", v.String)

	_, found = read(msMY)
	assert.False(t, found, "untranslated must be the ABSENCE of a row, omitted from export")
}

func TestTranslationConstraints(t *testing.T) {
	keyID := insertKey(t, "translation_constraints_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'first', 'test@you.co')`, keyID, enSG)
	require.NoError(t, err)

	t.Run("composite primary key forbids a second value for the same pair", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO translations (key_id, locale_id, value, updated_by)
			VALUES ($1, $2, 'second', 'test@you.co')`, keyID, enSG)
		requireRejected(t, err, "translations_pkey",
			"a duplicate (key, locale) translation")
	})

	t.Run("unknown render hint", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO translations (key_id, locale_id, value, render_hint, updated_by)
			VALUES ($1, $2, 'x', 'markdown', 'test@you.co')`,
			insertKey(t, "bad_render_hint_case"), enSG)
		requireRejected(t, err, "translations_render_hint_check",
			"render_hint 'markdown'")
	})

	t.Run("cascade removes translations with their key", func(t *testing.T) {
		// This key deliberately has NO history rows: since V1.08 a key with
		// history refuses a hard delete outright (see
		// TestHistoryRowsRefuseKeyHardDelete). What this subtest proves is the
		// half that still cascades — a value is content, not audit, and goes
		// with its key.
		doomed := insertKey(t, "cascade_case")
		_, err := testDB.Exec(`
			INSERT INTO translations (key_id, locale_id, value, updated_by)
			VALUES ($1, $2, 'x', 'test@you.co')`, doomed, enSG)
		require.NoError(t, err)

		_, err = testDB.Exec(`DELETE FROM keys WHERE id = $1`, doomed)
		require.NoError(t, err)

		var remaining int
		require.NoError(t, testDB.QueryRow(
			`SELECT count(*) FROM translations WHERE key_id = $1`, doomed).Scan(&remaining))
		assert.Zero(t, remaining)
	})
}

// TestHistoryRowsRefuseKeyHardDelete proves V1.08: an audit row must survive
// its subject, so a hard DELETE of a key that still has history is refused
// rather than quietly amplified into deleting the audit trail.
//
// No production path hard-deletes keys — the API soft-deletes to
// status = 'deleted' — so the only way to reach this constraint is a
// hand-typed DELETE in psql, which is exactly when the trail matters most.
func TestHistoryRowsRefuseKeyHardDelete(t *testing.T) {
	enSG := localeID(t, "en-SG")

	t.Run("translation history blocks the delete", func(t *testing.T) {
		keyID := insertKey(t, "history_guard_translation_case")
		_, err := testDB.Exec(`
			INSERT INTO translation_history (key_id, locale_id, value, version, source, changed_by)
			VALUES ($1, $2, 'v1', 1, 'ui', 'test@you.co')`, keyID, enSG)
		require.NoError(t, err)

		_, err = testDB.Exec(`DELETE FROM keys WHERE id = $1`, keyID)
		requireRejected(t, err, "translation_history_key_fkey",
			"a hard delete of a key that still has translation history")

		var remaining int
		require.NoError(t, testDB.QueryRow(
			`SELECT count(*) FROM translation_history WHERE key_id = $1`, keyID).Scan(&remaining))
		assert.Equal(t, 1, remaining, "the audit row must survive the attempt")
	})

	t.Run("key history blocks the delete", func(t *testing.T) {
		keyID := insertKey(t, "history_guard_key_case")
		_, err := testDB.Exec(`
			INSERT INTO key_history (key_id, name, platforms, status, version, source, changed_by)
			VALUES ($1, 'history_guard_key_case', ARRAY['flutter']::TEXT[], 'active', 1, 'ui', 'test@you.co')`,
			keyID)
		require.NoError(t, err)

		_, err = testDB.Exec(`DELETE FROM keys WHERE id = $1`, keyID)
		requireRejected(t, err, "key_history_key_fkey",
			"a hard delete of a key that still has key history")
	})

	t.Run("NO ACTION, not undeletable: removing the history in the open unblocks it", func(t *testing.T) {
		keyID := insertKey(t, "history_guard_explicit_case")
		_, err := testDB.Exec(`
			INSERT INTO translation_history (key_id, locale_id, value, version, source, changed_by)
			VALUES ($1, $2, 'v1', 1, 'ui', 'test@you.co')`, keyID, enSG)
		require.NoError(t, err)

		_, err = testDB.Exec(`DELETE FROM translation_history WHERE key_id = $1`, keyID)
		require.NoError(t, err)
		_, err = testDB.Exec(`DELETE FROM keys WHERE id = $1`, keyID)
		assert.NoError(t, err,
			"once the audit rows are deleted explicitly, the key delete is an ordinary delete")
	})
}

// TestOptimisticConcurrencyControl proves the mechanism the whole editing
// workflow depends on: a conditional UPDATE that affects zero rows IS the
// conflict signal, and the handler turns that into HTTP 409.
func TestOptimisticConcurrencyControl(t *testing.T) {
	keyID := insertKey(t, "occ_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'original', 'a@you.co')`, keyID, enSG)
	require.NoError(t, err)

	// Both editors read version 1.
	const casUpdate = `
		UPDATE translations SET value = $1, version = version + 1, updated_by = $2
		WHERE key_id = $3 AND locale_id = $4 AND version = $5`

	res, err := testDB.Exec(casUpdate, "Confirm", "a@you.co", keyID, enSG, 1)
	require.NoError(t, err)
	affected, _ := res.RowsAffected()
	require.EqualValues(t, 1, affected, "the first writer wins")

	res, err = testDB.Exec(casUpdate, "Proceed", "b@you.co", keyID, enSG, 1)
	require.NoError(t, err)
	affected, _ = res.RowsAffected()
	assert.EqualValues(t, 0, affected,
		"the second writer must affect zero rows — this is the 409 signal, not a silent overwrite")

	var value string
	var version int
	require.NoError(t, testDB.QueryRow(
		`SELECT value, version FROM translations WHERE key_id = $1 AND locale_id = $2`,
		keyID, enSG).Scan(&value, &version))
	assert.Equal(t, "Confirm", value, "the first writer's work must survive")
	assert.Equal(t, 2, version)
}

func TestBranchTranslationRemovalConsistency(t *testing.T) {
	var branchID int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO branches (name, created_by) VALUES ('removal-case', 'test@you.co')
		RETURNING id`).Scan(&branchID))

	keyID := insertKey(t, "branch_removal_case")
	enSG := localeID(t, "en-SG")

	t.Run("a removal must not carry a value", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO branch_translations
				(branch_id, key_id, locale_id, value, is_removed, base_master_version, updated_by)
			VALUES ($1, $2, $3, 'still here', TRUE, 1, 'test@you.co')`, branchID, keyID, enSG)
		requireRejected(t, err, "branch_translations_removed_value_check",
			"a tombstone delta that also carries a value")
	})

	t.Run("a non-removal must carry a value", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO branch_translations
				(branch_id, key_id, locale_id, value, is_removed, base_master_version, updated_by)
			VALUES ($1, $2, $3, NULL, FALSE, 1, 'test@you.co')`, branchID, keyID, enSG)
		requireRejected(t, err, "branch_translations_removed_value_check",
			"an edit delta with no value")
	})

	t.Run("base_master_version 0 records that no master row existed", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO branch_translations
				(branch_id, key_id, locale_id, value, base_master_version, updated_by)
			VALUES ($1, $2, $3, 'created on branch', 0, 'test@you.co')`, branchID, keyID, enSG)
		assert.NoError(t, err)
	})
}

// TestOneLiveMergeRequestPerBranch proves the partial unique index permits the
// reopen workflow: only non-terminal requests collide.
func TestOneLiveMergeRequestPerBranch(t *testing.T) {
	var branchID int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO branches (name, created_by) VALUES ('mr-case', 'test@you.co')
		RETURNING id`).Scan(&branchID))

	_, err := testDB.Exec(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, 'first', 'test@you.co')`, branchID)
	require.NoError(t, err)

	_, err = testDB.Exec(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, 'second', 'test@you.co')`, branchID)
	requireRejected(t, err, "idx_merge_requests_one_live_per_branch",
		"a second live merge request on the same branch")

	_, err = testDB.Exec(`UPDATE merge_requests SET status = 'rejected' WHERE branch_id = $1`, branchID)
	require.NoError(t, err)

	_, err = testDB.Exec(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, 'after rejection', 'test@you.co')`, branchID)
	assert.NoError(t, err, "a rejected request must not block a fresh one")
}

func TestAssetConstraints(t *testing.T) {
	// Each subtest needs its own VALID hash. Reusing one and appending a
	// character produces a 65-character string that trips the format check
	// instead of the constraint under test — the assertion would still pass,
	// for entirely the wrong reason.
	sha := func(seed string) string {
		sum := sha256.Sum256([]byte(seed))
		return hex.EncodeToString(sum[:])
	}

	t.Run("oversized upload rejected", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/big.png', $1, 'big.png', 'image/png', 10485761, 'test@you.co')`,
			sha("oversized"))
		requireRejected(t, err, "assets_size_check",
			"an asset one byte over the 10 MB ceiling")
	})

	t.Run("exactly at the ceiling accepted", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/exact.png', $1, 'exact.png', 'image/png', 10485760, 'test@you.co')`,
			sha("exact"))
		assert.NoError(t, err, "the limit is inclusive; an off-by-one here rejects valid uploads")
	})

	t.Run("zero-byte upload rejected", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/empty.png', $1, 'empty.png', 'image/png', 0, 'test@you.co')`,
			sha("empty"))
		requireRejected(t, err, "assets_size_check",
			"a zero-byte asset")
	})

	t.Run("disallowed content type rejected", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/x.svg', $1, 'x.svg', 'image/svg+xml', 100, 'test@you.co')`,
			sha("svg"))
		requireRejected(t, err, "assets_content_type_check",
			"an SVG upload")
	})

	t.Run("malformed sha256 rejected", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/y.png', 'NOT-A-HASH', 'y.png', 'image/png', 100, 'test@you.co')`)
		requireRejected(t, err, "assets_sha256_format_check",
			"a non-hex sha256")
	})

	t.Run("uppercase sha256 rejected", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/z.png', $1, 'z.png', 'image/png', 100, 'test@you.co')`,
			strings.ToUpper(sha("uppercase")))
		requireRejected(t, err, "assets_sha256_format_check",
			"an uppercase hash — one canonical encoding, or dedupe silently misses")
	})

	t.Run("identical content deduplicates", func(t *testing.T) {
		dup := sha("identical content")
		_, err := testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/first.png', $1, 'first.png', 'image/png', 100, 'test@you.co')`, dup)
		require.NoError(t, err)

		_, err = testDB.Exec(`
			INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, uploaded_by)
			VALUES ('screenshots/a/b/second.png', $1, 'second.png', 'image/png', 100, 'test@you.co')`, dup)
		requireRejected(t, err, "assets_project_sha256_unique",
			"a re-upload of identical bytes under a new key")
	})
}

func TestReleaseRollbackConsistency(t *testing.T) {
	_, err := testDB.Exec(`
		INSERT INTO releases (version, source, created_by, rolled_back_at)
		VALUES (9001, 'merge', 'test@you.co', now())`)
	requireRejected(t, err, "releases_rollback_consistency_check",
		"a rollback timestamp with no actor recorded")

	_, err = testDB.Exec(`
		INSERT INTO releases (version, source, created_by, rolled_back_at, rolled_back_by)
		VALUES (9002, 'merge', 'test@you.co', now(), 'admin@you.co')`)
	assert.NoError(t, err, "a fully-recorded rollback is valid")

	_, err = testDB.Exec(`
		INSERT INTO releases (version, source, created_by) VALUES (9002, 'merge', 'test@you.co')`)
	requireRejected(t, err, "releases_project_version_unique",
		"a duplicate release version")
}

func TestReleaseBundleSha256Format(t *testing.T) {
	var releaseID int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO releases (version, source, created_by) VALUES (9100, 'merge', 'test@you.co')
		RETURNING id`).Scan(&releaseID))

	_, err := testDB.Exec(`
		INSERT INTO release_bundles (release_id, locale_id, strings, sha256, key_count, byte_size)
		VALUES ($1, $2, '{}'::JSONB, 'deadbeef', 0, 0)`, releaseID, localeID(t, "en-SG"))
	requireRejected(t, err, "release_bundles_sha256_check",
		"a truncated sha256 as an ETag")
}

// TestEmailIsCaseInsensitive proves citext does the work, so no call site has
// to remember LOWER().
func TestEmailIsCaseInsensitive(t *testing.T) {
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ('Ashik.Saini@you.co', 'admin')`)
	require.NoError(t, err)

	_, err = testDB.Exec(`INSERT INTO users (email, role) VALUES ('ashik.saini@you.co', 'viewer')`)
	requireRejected(t, err, "users_email_unique",
		"the same email in different case")

	var role string
	require.NoError(t, testDB.QueryRow(
		`SELECT role FROM users WHERE email = 'ASHIK.SAINI@YOU.CO'`).Scan(&role))
	assert.Equal(t, "admin", role, "lookup must match regardless of case")
}

func TestUserRoleAndApiTokenConstraints(t *testing.T) {
	t.Run("unknown role rejected", func(t *testing.T) {
		_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ('x@you.co', 'superuser')`)
		requireRejected(t, err, "users_role_check",
			"role 'superuser'")
	})

	t.Run("token hash must be unique", func(t *testing.T) {
		hash := strings.Repeat("a", 64)
		_, err := testDB.Exec(`
			INSERT INTO api_tokens (name, token_sha256, token_prefix, created_by)
			VALUES ('ci', $1, 'ul10n_aaaa', 'test@you.co')`, hash)
		require.NoError(t, err)

		_, err = testDB.Exec(`
			INSERT INTO api_tokens (name, token_sha256, token_prefix, created_by)
			VALUES ('ci-2', $1, 'ul10n_aaaa', 'test@you.co')`, hash)
		requireRejected(t, err, "api_tokens_sha256_unique",
			"a duplicate token hash")
	})

	t.Run("unknown scope rejected", func(t *testing.T) {
		_, err := testDB.Exec(`
			INSERT INTO api_tokens (name, token_sha256, token_prefix, scope, created_by)
			VALUES ('bad', $1, 'ul10n_bbbb', 'admin', 'test@you.co')`, strings.Repeat("b", 64))
		requireRejected(t, err, "api_tokens_scope_check",
			"scope 'admin'")
	})
}
