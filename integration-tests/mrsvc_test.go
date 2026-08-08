package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/branchsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mrsvc"
)

func newMRSvc(t *testing.T) *mrsvc.Service {
	t.Helper()
	conn := testGORM(t)
	tx := database.ProvideTransactional(conn)

	merge := mergesvc.ProvideService(tx,
		repository.ProvideBranchRepository(conn),
		repository.ProvideMergeRequestRepository(conn),
		repository.ProvideLocaleRepository(conn),
		repository.ProvideReleaseRepository(conn),
		repository.ProvideExportRowReader(conn))

	return mrsvc.ProvideService(tx,
		repository.ProvideBranchRepository(conn),
		repository.ProvideMergeRequestRepository(conn),
		repository.ProvideLocaleRepository(conn),
		merge,
		repository.ProvideAuditRepository(conn))
}

// mergeAndQuarantine performs a merge and rolls its release back afterwards.
//
// A merge materialises a bundle for EVERY locale in the same transaction, which
// makes it globally servable — and the OTA tests assert on exactly which
// release a client receives for a given locale. The package applies migrations
// once and never resets between cases, so without this every merge here would
// silently become the newest eligible release for all six locales and the OTA
// suite would start failing for reasons that have nothing to do with OTA.
//
// Rolling back rather than deleting: rolled_back_at is the production kill
// switch, so this exercises a real state rather than inventing a cleanup path
// the schema does not have.
func mergeAndQuarantine(
	t *testing.T, mrs *mrsvc.Service, mrID int64, actor string,
) (*mergesvc.Result, error) {
	t.Helper()

	result, err := mrs.Merge(context.Background(), mrID, actor, "req-1")
	if result != nil {
		version := result.ReleaseVersion
		t.Cleanup(func() {
			_, cleanupErr := testDB.Exec(`
				UPDATE releases SET rolled_back_at = now(), rolled_back_by = 'test-cleanup'
				 WHERE version = $1 AND rolled_back_at IS NULL`, version)
			require.NoError(t, cleanupErr)
		})
	}
	return result, err
}

// branchWithEdit opens a branch and puts one value change on it.
func branchWithEdit(t *testing.T, keys *keysvc.Service, branches *branchsvc.Service, keyID int64, value string) repository.Branch {
	t.Helper()
	ctx := context.Background()

	branch, err := branches.Create(ctx, uniqueName(t, "mr"), "", testActor, "req-1")
	require.NoError(t, err)

	_, err = keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: keyID, LocaleCode: "en-SG", Branch: branch.Name, Value: value,
	}, testActor, "req-1")
	require.NoError(t, err)

	return branch
}

// TestMergeRequestWorkflowTransitions walks the whole state machine and, more
// importantly, the moves it must refuse.
func TestMergeRequestWorkflowTransitions(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "workflow"))
	branch := branchWithEdit(t, keys, branches, key.ID, "reviewed copy")

	mr, err := mrs.Create(ctx, branch.Name, "please review", testActor, "req-1")
	require.NoError(t, err)
	assert.Equal(t, repository.MRStatusOpen, mr.Status)

	t.Run("a branch may hold only one live request", func(t *testing.T) {
		_, err := mrs.Create(ctx, branch.Name, "another", testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, repository.ErrLiveMergeRequestExists)
	})

	t.Run("request-changes needs a reason", func(t *testing.T) {
		// Sending work back without saying what is wrong is not a review.
		_, err := mrs.Review(ctx, mr.ID, mrsvc.ActionRequestChanges, "", testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, mrsvc.ErrBadRequest)
	})

	t.Run("changes requested, then approved", func(t *testing.T) {
		moved, err := mrs.Review(ctx, mr.ID, mrsvc.ActionRequestChanges,
			"the Thai string overflows the button", testActor, "req-1")
		require.NoError(t, err)
		assert.Equal(t, repository.MRStatusChangesRequested, moved.Status)

		moved, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
		require.NoError(t, err)
		assert.Equal(t, repository.MRStatusApproved, moved.Status)

		detail, err := mrs.Get(ctx, mr.ID)
		require.NoError(t, err)
		require.NotNil(t, detail.MergeRequest.ApprovedBy)
		assert.Equal(t, "approver@you.co", *detail.MergeRequest.ApprovedBy)
		// approved_at is what the merge compares against the branch's
		// last_edited_at. A status change that left it NULL would make the
		// staleness check inert.
		assert.NotNil(t, detail.MergeRequest.ApprovedAt)
	})

	t.Run("approving twice is refused, not repeated", func(t *testing.T) {
		_, err := mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, mrsvc.ErrNotLive)
	})

	t.Run("an unknown action is a bad request", func(t *testing.T) {
		_, err := mrs.Review(ctx, mr.ID, "yolo", "", testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, mrsvc.ErrBadRequest)
	})

	t.Run("reject is terminal and reopen brings it back", func(t *testing.T) {
		moved, err := mrs.Review(ctx, mr.ID, mrsvc.ActionReject, "not shipping this", testActor, "req-1")
		require.NoError(t, err)
		assert.Equal(t, repository.MRStatusRejected, moved.Status)

		_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", testActor, "req-1")
		assert.ErrorIs(t, err, mrsvc.ErrNotLive, "a rejected request cannot be approved")

		moved, err = mrs.Review(ctx, mr.ID, mrsvc.ActionReopen, "", testActor, "req-1")
		require.NoError(t, err)
		assert.Equal(t, repository.MRStatusOpen, moved.Status)
	})

	t.Run("the timeline records every transition", func(t *testing.T) {
		detail, err := mrs.Get(ctx, mr.ID)
		require.NoError(t, err)

		var events []string
		for _, e := range detail.Events {
			events = append(events, e.Event)
		}
		assert.Equal(t, []string{
			"created", "changes_requested", "approved", "rejected", "reopened",
		}, events)
	})
}

// TestReopenIsRefusedWhenAnotherRequestIsLive.
//
// idx_merge_requests_one_live_per_branch is partial over the non-terminal
// states, so this is a real constraint violation. It must surface as a 409 a
// human can act on, not as a 500.
func TestReopenIsRefusedWhenAnotherRequestIsLive(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "two_live"))
	branch := branchWithEdit(t, keys, branches, key.ID, "v1")

	first, err := mrs.Create(ctx, branch.Name, "first", testActor, "req-1")
	require.NoError(t, err)
	_, err = mrs.Review(ctx, first.ID, mrsvc.ActionReject, "no", testActor, "req-1")
	require.NoError(t, err)

	second, err := mrs.Create(ctx, branch.Name, "second", testActor, "req-1")
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)

	_, err = mrs.Review(ctx, first.ID, mrsvc.ActionReopen, "", testActor, "req-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrLiveMergeRequestExists)
}

// TestMergeRefusesAnUnapprovedRequest.
func TestMergeRefusesAnUnapprovedRequest(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "unapproved"))
	branch := branchWithEdit(t, keys, branches, key.ID, "v1")

	mr, err := mrs.Create(ctx, branch.Name, "unapproved", testActor, "req-1")
	require.NoError(t, err)

	_, err = mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	require.Error(t, err)
	assert.ErrorIs(t, err, mergesvc.ErrNotApproved)
}

// TestApprovalIsInvalidatedByALaterBranchEdit.
//
// THE property the whole review workflow rests on: you cannot get a diff
// approved and then quietly change it. The invalidation happens on the branch
// write itself, and the merge checks again inside its own transaction.
func TestApprovalIsInvalidatedByALaterBranchEdit(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "stale"))
	branch := branchWithEdit(t, keys, branches, key.ID, "approved copy")

	mr, err := mrs.Create(ctx, branch.Name, "review me", testActor, "req-1")
	require.NoError(t, err)
	_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
	require.NoError(t, err)

	// The author sneaks in a different string after sign-off.
	_, err = keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: key.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "something else entirely",
	}, testActor, "req-1")
	require.NoError(t, err)

	detail, err := mrs.Get(ctx, mr.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.MRStatusOpen, detail.MergeRequest.Status,
		"the approval must be withdrawn automatically")
	assert.Nil(t, detail.MergeRequest.ApprovedBy)

	// And the merge refuses regardless of what the caller believed.
	_, err = mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	require.Error(t, err)
	assert.ErrorIs(t, err, mergesvc.ErrNotApproved)

	// The timeline says who did it and why: 'system', because no human made
	// this transition.
	var invalidations []repository.MergeRequestEvent
	for _, e := range detail.Events {
		if e.Event == "approval_invalidated" {
			invalidations = append(invalidations, e)
		}
	}
	require.Len(t, invalidations, 1)
	assert.Equal(t, "system", invalidations[0].Actor)
}

// TestConflictsReportAllThreeKinds.
//
// Values, metadata and name collisions are genuinely different, and the third
// carries no resolution because choosing a side cannot fix it — one of the two
// keys has to be renamed.
func TestConflictsReportAllThreeKinds(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	valueKey := createTestKey(t, keys, uniqueName(t, "value"))
	metaKey := createTestKey(t, keys, uniqueName(t, "meta"))
	collidingKey := createTestKey(t, keys, uniqueName(t, "collide"))
	takenName := uniqueName(t, "already_taken")
	createTestKey(t, keys, takenName)

	setPortalValue(t, keys, valueKey.ID, "en-SG", "master v1", 0)

	branch, err := branches.Create(ctx, uniqueName(t, "three_kinds"), "", testActor, "req-1")
	require.NoError(t, err)

	_, err = keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: valueKey.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "branch copy",
	}, testActor, "req-1")
	require.NoError(t, err)
	_, err = keys.UpdateKey(ctx, keysvc.UpdateKeyRequest{
		KeyID: metaKey.ID, Branch: branch.Name, Description: strPtr("branch note"),
	}, testActor, "req-1")
	require.NoError(t, err)
	// The branch renames a key onto a name master already holds.
	_, err = keys.UpdateKey(ctx, keysvc.UpdateKeyRequest{
		KeyID: collidingKey.ID, Branch: branch.Name, Name: &takenName,
	}, testActor, "req-1")
	require.NoError(t, err)

	// Master moves under both the value and the metadata.
	setPortalValue(t, keys, valueKey.ID, "en-SG", "master v2", 1)
	_, err = keys.UpdateKey(ctx, keysvc.UpdateKeyRequest{
		KeyID: metaKey.ID, Description: strPtr("master note"),
	}, testActor, "req-1")
	require.NoError(t, err)

	mr, err := mrs.Create(ctx, branch.Name, "all three", testActor, "req-1")
	require.NoError(t, err)

	conflicts, err := mrs.Conflicts(ctx, mr.ID)
	require.NoError(t, err)

	require.Len(t, conflicts.Values, 1)
	assert.Equal(t, "branch copy", conflicts.Values[0].Mine)
	assert.Equal(t, "master v2", conflicts.Values[0].Theirs)
	assert.True(t, conflicts.Values[0].TheirsFound)

	require.Len(t, conflicts.Meta, 1)
	assert.Equal(t, metaKey.ID, conflicts.Meta[0].KeyID)

	require.Len(t, conflicts.Collisions, 1)
	assert.Equal(t, takenName, conflicts.Collisions[0].Name)

	assert.Equal(t, 2, conflicts.Unresolved, "collisions are not resolvable and are not counted")
	assert.False(t, conflicts.Mergeable())

	t.Run("resolving both leaves the collision blocking", func(t *testing.T) {
		remaining, err := mrs.Resolve(ctx, mr.ID, []mrsvc.Resolution{
			{KeyID: valueKey.ID, LocaleCode: "en-SG", Choice: "mine"},
			{KeyID: metaKey.ID, Choice: "master"},
		}, "approver@you.co", "req-1")
		require.NoError(t, err)

		assert.Equal(t, 0, remaining.Unresolved)
		assert.Len(t, remaining.Collisions, 1)
		assert.False(t, remaining.Mergeable(),
			"a name collision cannot be resolved by choosing a side")
	})

	t.Run("the merge refuses with the collisions in the error", func(t *testing.T) {
		_, err := mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
		require.NoError(t, err)

		_, err = mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
		require.Error(t, err)
		assert.ErrorIs(t, err, mergesvc.ErrNameCollision)

		var collision *mergesvc.CollisionError
		require.ErrorAs(t, err, &collision)
		require.Len(t, collision.Collisions, 1)
		assert.Equal(t, takenName, collision.Collisions[0].Name)
	})
}

// TestMergeRefusesUnresolvedConflictsWithTheListAttached.
//
// "There were 3 conflicts" tells a reviewer nothing they can act on. The
// blocking rows travel with the error.
func TestMergeRefusesUnresolvedConflictsWithTheListAttached(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "unresolved"))
	setPortalValue(t, keys, key.ID, "en-SG", "master v1", 0)

	branch := branchWithEdit(t, keys, branches, key.ID, "branch copy")
	setPortalValue(t, keys, key.ID, "en-SG", "master v2", 1)

	mr, err := mrs.Create(ctx, branch.Name, "conflicted", testActor, "req-1")
	require.NoError(t, err)
	_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
	require.NoError(t, err)

	_, err = mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	require.Error(t, err)
	assert.ErrorIs(t, err, mergesvc.ErrUnresolvedConflicts)

	var unresolved *mergesvc.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	require.Len(t, unresolved.Values, 1)
	assert.Equal(t, "branch copy", unresolved.Values[0].Mine)
	assert.Equal(t, "master v2", unresolved.Values[0].Theirs)

	// Nothing was applied.
	current, err := keys.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.Equal(t, "master v2", current.Keys[0].Values[current.Locales[0].ID].Value)
}

// TestASuccessfulMergeAppliesAndCutsARelease: the happy path, end to end.
func TestASuccessfulMergeAppliesAndCutsARelease(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "happy"))
	branch := branchWithEdit(t, keys, branches, key.ID, "shipped copy")

	mr, err := mrs.Create(ctx, branch.Name, "ship it", testActor, "req-1")
	require.NoError(t, err)
	_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
	require.NoError(t, err)

	result, err := mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	require.NoError(t, err)
	assert.Greater(t, result.ReleaseVersion, int64(0))
	assert.Equal(t, 1, result.ValuesApplied)
	// Every locale gets a bundle in the same transaction, which is what makes
	// the export and OTA endpoints read-only over identical rows.
	assert.Equal(t, 6, result.BundlesWritten)

	master, err := keys.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.Equal(t, "shipped copy", master.Keys[0].Values[master.Locales[0].ID].Value)

	t.Run("a merged request cannot be merged again", func(t *testing.T) {
		_, err := mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
		require.Error(t, err)
		assert.ErrorIs(t, err, mrsvc.ErrNotLive)
	})

	t.Run("the merged branch is closed to further edits", func(t *testing.T) {
		_, err := keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
			KeyID: key.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "too late",
		}, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, keysvc.ErrBranchNotOpen)
	})
}

// TestMergeAppliesTombstonesAsDeletions.
//
// A branch that removes a translation must DELETE master's row, not blank it.
// Blanking would turn an untranslated key into a deliberate empty string, which
// the export treats as content.
func TestMergeAppliesTombstonesAsDeletions(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "tombstone_merge"))
	setPortalValue(t, keys, key.ID, "en-SG", "will be removed", 0)

	branch, err := branches.Create(ctx, uniqueName(t, "tomb_mr"), "", testActor, "req-1")
	require.NoError(t, err)
	require.NoError(t, keys.DeleteTranslation(ctx, key.ID, "en-SG", branch.Name, nil, testActor, "req-1"))

	mr, err := mrs.Create(ctx, branch.Name, "remove it", testActor, "req-1")
	require.NoError(t, err)
	_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
	require.NoError(t, err)
	_, err = mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	require.NoError(t, err)

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM translations WHERE key_id = $1`, key.ID).Scan(&count))
	assert.Equal(t, 0, count, "the row must be gone, not blanked")
}

// TestResolutionsRefuseWhatTheyCannotRecord.
func TestResolutionsRefuseWhatTheyCannotRecord(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "bad_resolutions"))
	branch := branchWithEdit(t, keys, branches, key.ID, "v1")
	mr, err := mrs.Create(ctx, branch.Name, "x", testActor, "req-1")
	require.NoError(t, err)

	cases := []struct {
		name string
		in   []mrsvc.Resolution
	}{
		{"empty set", nil},
		{"no key", []mrsvc.Resolution{{LocaleCode: "en-SG", Choice: "mine"}}},
		{"unknown choice", []mrsvc.Resolution{{KeyID: key.ID, LocaleCode: "en-SG", Choice: "theirs"}}},
		{"unknown locale", []mrsvc.Resolution{{KeyID: key.ID, LocaleCode: "fr-FR", Choice: "mine"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mrs.Resolve(ctx, mr.ID, tc.in, testActor, "req-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, mrsvc.ErrBadRequest)
		})
	}
}

// TestMergeRequestOnAClosedBranchIsRefused: reviewing a closed branch would be
// reviewing a decision already taken.
func TestMergeRequestOnAClosedBranchIsRefused(t *testing.T) {
	ctx := context.Background()
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	name := uniqueName(t, "closed_branch_mr")
	_, err := branches.Create(ctx, name, "", testActor, "req-1")
	require.NoError(t, err)
	_, err = branches.Close(ctx, name, testActor, "req-1")
	require.NoError(t, err)

	_, err = mrs.Create(ctx, name, "too late", testActor, "req-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, mrsvc.ErrNotLive)
}

// TestMergeRequestListFiltersByStatus.
func TestMergeRequestListFiltersByStatus(t *testing.T) {
	ctx := context.Background()
	mrs := newMRSvc(t)

	_, err := mrs.List(ctx, "in_review")
	require.Error(t, err)
	assert.ErrorIs(t, err, mrsvc.ErrBadRequest)

	all, err := mrs.List(ctx, "")
	require.NoError(t, err)

	merged, err := mrs.List(ctx, repository.MRStatusMerged)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(merged), len(all))
	for _, m := range merged {
		assert.Equal(t, repository.MRStatusMerged, m.Status)
		assert.NotEmpty(t, m.BranchName, "the listing carries the branch name so no lookup per row is needed")
	}
}

var _ = model.PlatformFlutter
