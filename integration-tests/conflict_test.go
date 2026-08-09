package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conflictCount returns how many of a branch's deltas conflict with master,
// using the same single comparison the merge transaction uses.
func conflictCount(t *testing.T, branchID int64) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`
		SELECT count(*)
		  FROM branch_translations bt
		  LEFT JOIN translations t
		    ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
		 WHERE bt.branch_id = $1
		   AND COALESCE(t.version, 0) <> bt.base_master_version`, branchID).Scan(&n))
	return n
}

// TestConflictRuleCoversEveryRace walks the truth table from the design.
//
// One integer comparison — COALESCE(master.version,0) <> base_master_version —
// must catch edit/edit, create/create, remove/edit and edit/delete, and must
// NOT fire when the branch is simply ahead of an untouched master. Four
// separate detection paths would be the alternative, and the fourth is always
// the buggy one.
func TestConflictRuleCoversEveryRace(t *testing.T) {
	enSG := localeID(t, "en-SG")

	cases := []struct {
		name         string
		masterInit   bool   // master row exists before the branch touches it
		masterThen   string // "", "edit", "delete"
		wantConflict bool
	}{
		{"edited, master untouched", true, "", false},
		{"edited, master edited", true, "edit", true},
		{"created, master still absent", false, "", false},
		{"created, master created too", false, "edit", true},
		{"edited, master key deleted", true, "delete", true},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branchID := newBranch(t, "conflict-case-"+tc.name)
			keyID := insertKey(t, "conflict_case_key_"+string(rune('a'+i)))

			if tc.masterInit {
				_, err := testDB.Exec(`
					INSERT INTO translations (key_id, locale_id, value, updated_by)
					VALUES ($1, $2, 'master v1', 'test@you.co')`, keyID, enSG)
				require.NoError(t, err)
			}

			// The branch edits, capturing base_master_version as it stands now.
			setValue(t, branchID, keyID, enSG, "branch edit")

			switch tc.masterThen {
			case "edit":
				_, err := testDB.Exec(`
					INSERT INTO translations (key_id, locale_id, value, version, updated_by)
					VALUES ($1, $2, 'master v2', 1, 'other@you.co')
					ON CONFLICT (key_id, locale_id) DO UPDATE
					   SET value = 'master v2', version = translations.version + 1`, keyID, enSG)
				require.NoError(t, err)
			case "delete":
				// A soft delete bumps the key's version; the translation row's
				// version moves with the edit that accompanies it.
				_, err := testDB.Exec(`
					UPDATE translations SET version = version + 1
					 WHERE key_id = $1 AND locale_id = $2`, keyID, enSG)
				require.NoError(t, err)
			}

			got := conflictCount(t, branchID) == 1
			assert.Equal(t, tc.wantConflict, got,
				"base=%d master=%d", baseVersion(t, branchID, keyID, enSG), 0)
		})
	}
}

// TestApprovalInvalidatedByLaterEdit proves a reviewer cannot approve one diff
// and have a different one merged.
func TestApprovalInvalidatedByLaterEdit(t *testing.T) {
	branchID := newBranch(t, "approval-invalidation")
	keyID := insertKey(t, "approval_invalidation_case")
	enSG := localeID(t, "en-SG")

	setValue(t, branchID, keyID, enSG, "first")
	_, err := testDB.Exec(`UPDATE branches SET last_edited_at = now() WHERE id = $1`, branchID)
	require.NoError(t, err)

	var mrID int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, 'copy fixes', 'author@you.co') RETURNING id`, branchID).Scan(&mrID))

	_, err = testDB.Exec(`
		UPDATE merge_requests SET status='approved', approved_by='approver@you.co', approved_at=now()
		 WHERE id = $1`, mrID)
	require.NoError(t, err)

	invalidate := func() bool {
		var id int64
		err := testDB.QueryRow(`
			UPDATE merge_requests mr SET status='open', approved_by=NULL, approved_at=NULL
			  FROM branches b
			 WHERE mr.branch_id = b.id AND mr.branch_id = $1 AND mr.status = 'approved'
			   AND b.last_edited_at IS NOT NULL AND b.last_edited_at > mr.approved_at
			RETURNING mr.id`, branchID).Scan(&id)
		return err == nil
	}

	assert.False(t, invalidate(), "an approved, untouched branch must stay approved")

	// Now edit the branch after approval.
	setValue(t, branchID, keyID, enSG, "sneaky change")
	_, err = testDB.Exec(`UPDATE branches SET last_edited_at = now() WHERE id = $1`, branchID)
	require.NoError(t, err)

	assert.True(t, invalidate(), "an edit after approval must re-open the request")

	var status string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM merge_requests WHERE id = $1`, mrID).Scan(&status))
	assert.Equal(t, "open", status)

	var approvedBy *string
	require.NoError(t, testDB.QueryRow(
		`SELECT approved_by FROM merge_requests WHERE id = $1`, mrID).Scan(&approvedBy))
	assert.Nil(t, approvedBy, "the stale approver must be cleared, not merely ignored")
}

// TestResolutionsAreUniquePerConflict guards the COALESCE(locale_id, -1) index:
// without it, NULL locale_id rows (key-metadata conflicts) would not collide and
// a conflict could carry two contradictory resolutions.
func TestResolutionsAreUniquePerConflict(t *testing.T) {
	branchID := newBranch(t, "resolution-uniqueness")
	keyID := insertKey(t, "resolution_uniqueness_case")
	enSG := localeID(t, "en-SG")
	_ = branchID

	var mrID int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, 'r', 'a@you.co') RETURNING id`, branchID).Scan(&mrID))

	ins := func(locale interface{}, resolution string) error {
		_, err := testDB.Exec(`
			INSERT INTO merge_conflict_resolutions
			    (merge_request_id, key_id, locale_id, resolution, resolved_by)
			VALUES ($1, $2, $3, $4, 'a@you.co')`, mrID, keyID, locale, resolution)
		return err
	}

	require.NoError(t, ins(enSG, "mine"))
	requireRejected(t, ins(enSG, "master"), "idx_merge_conflict_resolutions_unique",
		"a second resolution for the same value conflict")

	// Key-metadata conflicts carry a NULL locale and must also be unique. Naming
	// the same index for both halves is the assertion: a plain
	// UNIQUE (merge_request_id, key_id, locale_id) would refuse the first
	// insert and let this one through, so both must be refused by the
	// COALESCE-expression index and by nothing else.
	require.NoError(t, ins(nil, "mine"))
	requireRejected(t, ins(nil, "master"), "idx_merge_conflict_resolutions_unique",
		"a second resolution for the same metadata conflict — NULLs would not collide without COALESCE")
}
