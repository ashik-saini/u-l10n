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

	t.Run("a merged request cannot be closed", func(t *testing.T) {
		// merged → nothing is the documented invariant. A close that landed
		// would erase the record that this request shipped a release.
		_, err := mrs.Review(ctx, mr.ID, mrsvc.ActionClose, "", testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, mrsvc.ErrNotLive, "a 409 the caller can act on, not a success")

		var status string
		require.NoError(t, testDB.QueryRow(
			`SELECT status FROM merge_requests WHERE id = $1`, mr.ID).Scan(&status))
		assert.Equal(t, repository.MRStatusMerged, status, "the merged state must survive the attempt")
	})

	t.Run("the merged key's timeline shows the merge", func(t *testing.T) {
		// The History endpoint must not lie after a merge: the value this
		// merge landed is an event on the key's timeline, source 'merge'.
		_, values, err := keys.History(ctx, key.ID, "en-SG", 10)
		require.NoError(t, err)

		var mergeEntries []repository.TranslationHistoryEntry
		for _, e := range values {
			if e.Source == "merge" {
				mergeEntries = append(mergeEntries, e)
			}
		}
		require.Len(t, mergeEntries, 1, "one merge, one history row")
		require.NotNil(t, mergeEntries[0].Value)
		assert.Equal(t, "shipped copy", *mergeEntries[0].Value)
		assert.Equal(t, "approver@you.co", mergeEntries[0].ChangedBy,
			"attributed to the actor who merged, not the branch author")
		require.NotNil(t, mergeEntries[0].BranchID)
		assert.Equal(t, branch.ID, *mergeEntries[0].BranchID)
	})

	t.Run("the merged branch is closed to further edits", func(t *testing.T) {
		_, err := keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
			KeyID: key.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "too late",
		}, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, keysvc.ErrBranchNotOpen)
	})
}

// TestMergeLandsAKeyCreatedOnABranch is the regression test for the silent
// drop, end to end through the real merge transaction.
//
// The rule it pins down: a merge involving a branch-created key must either
// land that key on master or refuse loudly. Reporting success while the key
// never arrives is the one outcome that is unacceptable, because nothing
// downstream can detect it — the merge cuts a release, the release materialises
// bundles, and every consumer afterwards reads a corpus that is quietly missing
// a key somebody wrote and somebody else approved.
//
// Against the pre-fix code this fails at the first line: creating a key on a
// branch was refused outright, precisely because applyKeyMetaSQL would have
// skipped a NULL-key_id delta and dropped it.
func TestMergeLandsAKeyCreatedOnABranch(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)
	exportRows := repository.ProvideExportRowReader(testGORM(t))
	enSG := localeID(t, "en-SG")

	branch, err := branches.Create(ctx, uniqueName(t, "new_key"), "", testActor, "req-1")
	require.NoError(t, err)

	name := uniqueName(t, "created_on_branch")
	created, err := keys.CreateKey(ctx, branch.Name, keysvc.CreateKeyRequest{
		Name:      name,
		Platforms: []model.Platform{model.PlatformFlutter},
	}, testActor, "req-1")
	require.NoError(t, err)

	_, err = keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: created.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "brand new copy",
	}, testActor, "req-1")
	require.NoError(t, err)

	// Before the merge the key reaches nobody. This is the claim the draft model
	// rests on, and it is checked against the SAME reader the merge uses to
	// materialise bundles — so it covers the export endpoint and the OTA
	// payload at once.
	inExport := func() bool {
		rows, err := exportRows.ForExport(ctx, nil, enSG, model.PlatformFlutter)
		require.NoError(t, err)
		for _, row := range rows {
			if row.Key == name {
				return true
			}
		}
		return false
	}
	assert.False(t, inExport(), "a draft must not reach the export or any OTA bundle")

	mr, err := mrs.Create(ctx, branch.Name, "a new string", testActor, "req-1")
	require.NoError(t, err)

	conflicts, err := mrs.Conflicts(ctx, mr.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, conflicts.Unresolved, "a new key conflicts with nothing")
	assert.Empty(t, conflicts.Collisions, "and it must not collide with its own draft row")
	assert.True(t, conflicts.Mergeable())

	_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
	require.NoError(t, err)

	result, err := mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	require.NoError(t, err)
	assert.Equal(t, 1, result.KeysApplied, "the promotion is a metadata delta like any other")

	// THE ASSERTION. The key is on master, active, carrying its value.
	var status string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM keys WHERE id = $1`, created.ID).Scan(&status))
	assert.Equal(t, "active", status,
		"the merge reported success, so the key must actually be on master")

	master, err := keys.Get(ctx, created.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	cell := master.Keys[0].Values[master.Locales[0].ID]
	assert.True(t, cell.Found, "the value the author wrote must have landed with the key")
	assert.Equal(t, "brand new copy", cell.Value)

	assert.True(t, inExport(), "and it is part of the corpus from this release onward")

	// The release cut by this merge carries it too, which is what the OTA
	// endpoint serves — read-only over these precomputed rows.
	var bundle string
	require.NoError(t, testDB.QueryRow(`
		SELECT rb.strings::text
		  FROM release_bundles rb
		  JOIN releases r ON r.id = rb.release_id
		 WHERE r.version = $1 AND rb.locale_id = $2`,
		result.ReleaseVersion, enSG).Scan(&bundle))
	assert.Contains(t, bundle, name, "the bundle materialised in the same transaction must hold it")
}

// TestMergeRefusesABranchCreatedKeyWhoseNameMasterTook is the other half of the
// rule: when the key cannot land, the merge must say so rather than skip it.
//
// Two branches each create a key called the same thing. Both drafts coexist —
// idx_keys_name_active is partial and does not see drafts — and the first merge
// promotes one of them to active. The second merge must now refuse, loudly and
// before applying anything, rather than dying on the unique index halfway
// through or quietly dropping the key.
func TestMergeRefusesABranchCreatedKeyWhoseNameMasterTook(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrs := newMRSvc(t)

	name := uniqueName(t, "contested")

	merge := func(branch repository.Branch) (*mergesvc.Result, error) {
		mr, err := mrs.Create(ctx, branch.Name, "claim the name", testActor, "req-1")
		require.NoError(t, err)
		_, err = mrs.Review(ctx, mr.ID, mrsvc.ActionApprove, "", "approver@you.co", "req-1")
		require.NoError(t, err)
		return mergeAndQuarantine(t, mrs, mr.ID, "approver@you.co")
	}

	var created []repository.Branch
	for i := 0; i < 2; i++ {
		branch, err := branches.Create(ctx, uniqueName(t, "claim"), "", testActor, "req-1")
		require.NoError(t, err)
		_, err = keys.CreateKey(ctx, branch.Name, keysvc.CreateKeyRequest{
			Name: name, Platforms: []model.Platform{model.PlatformFlutter},
		}, testActor, "req-1")
		require.NoError(t, err, "two drafts may share a name; only one may become active")
		created = append(created, branch)
	}

	_, err := merge(created[0])
	require.NoError(t, err, "the first claim wins")

	_, err = merge(created[1])
	require.Error(t, err, "the second must NOT report success")
	assert.ErrorIs(t, err, mergesvc.ErrNameCollision)

	var collision *mergesvc.CollisionError
	require.ErrorAs(t, err, &collision)
	require.Len(t, collision.Collisions, 1)
	assert.Equal(t, name, collision.Collisions[0].Name,
		"the reviewer is told which name, so one of them can be renamed")

	// And nothing was half-applied: exactly one active key holds the name.
	var active int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM keys WHERE name = $1 AND status = 'active'`, name).Scan(&active))
	assert.Equal(t, 1, active)
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
		// The branch edited this pair but master never moved: there is no
		// conflict to decide. Recording 'master' anyway would make the merge
		// silently discard a delta nobody disputed.
		{"value pair not in conflict", []mrsvc.Resolution{{KeyID: key.ID, LocaleCode: "en-SG", Choice: "master"}}},
		{"metadata not in conflict", []mrsvc.Resolution{{KeyID: key.ID, Choice: "master"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mrs.Resolve(ctx, mr.ID, tc.in, testActor, "req-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, mrsvc.ErrBadRequest)
		})
	}

	// And nothing was recorded by the refused attempts: the resolution set is
	// all-or-nothing, so a rejected entry must roll the whole call back.
	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM merge_conflict_resolutions WHERE merge_request_id = $1`,
		mr.ID).Scan(&count))
	assert.Zero(t, count)
}

// TestSetStatusIsACompareAndSwap pins the repository guard the workflow rests
// on. The service pre-checks the state, but that read holds no lock — a merge
// can commit between it and the UPDATE. The state predicate inside SetStatus
// is what actually stops a racing close from overwriting 'merged', and a miss
// must surface as the stale-status sentinel, never as a silent success.
func TestSetStatusIsACompareAndSwap(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)
	mrsRepo := repository.ProvideMergeRequestRepository(testGORM(t))

	key := createTestKey(t, keys, uniqueName(t, "cas"))
	branch := branchWithEdit(t, keys, branches, key.ID, "v1")

	mr, err := mrsRepo.Create(ctx, nil, branch.ID, "cas", testActor)
	require.NoError(t, err)

	// The merge wins the race: the request is merged now, though the closing
	// caller still believes it is open.
	_, err = testDB.Exec(
		`UPDATE merge_requests SET status = 'merged', merged_at = now() WHERE id = $1`, mr.ID)
	require.NoError(t, err)

	err = mrsRepo.SetStatus(ctx, nil, mr.ID, repository.MRStatusClosed,
		[]string{repository.MRStatusOpen, repository.MRStatusApproved, repository.MRStatusChangesRequested},
		testActor, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrStaleMergeRequestStatus)

	var status string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM merge_requests WHERE id = $1`, mr.ID).Scan(&status))
	assert.Equal(t, repository.MRStatusMerged, status,
		"the lost race must not overwrite the merged state")

	// No phantom 'closed' event either: the timeline records what happened.
	events, err := mrsRepo.Events(ctx, nil, mr.ID)
	require.NoError(t, err)
	for _, e := range events {
		assert.NotEqual(t, "closed", e.Event,
			"a refused transition must leave no event claiming it happened")
	}

	t.Run("the matching state still transitions", func(t *testing.T) {
		branch2 := branchWithEdit(t, keys, branches, key.ID, "v2")
		ok, err := mrsRepo.Create(ctx, nil, branch2.ID, "cas-ok", testActor)
		require.NoError(t, err)

		require.NoError(t, mrsRepo.SetStatus(ctx, nil, ok.ID, repository.MRStatusClosed,
			[]string{repository.MRStatusOpen, repository.MRStatusApproved, repository.MRStatusChangesRequested},
			testActor, ""))
		var status string
		require.NoError(t, testDB.QueryRow(
			`SELECT status FROM merge_requests WHERE id = $1`, ok.ID).Scan(&status))
		assert.Equal(t, repository.MRStatusClosed, status)
	})
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
