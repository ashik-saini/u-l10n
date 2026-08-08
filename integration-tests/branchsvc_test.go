package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/branchsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
)

func newBranchSvc(t *testing.T) *branchsvc.Service {
	t.Helper()
	conn := testGORM(t)
	return branchsvc.ProvideService(
		database.ProvideTransactional(conn),
		repository.ProvideBranchRepository(conn),
		repository.ProvideAuditRepository(conn),
	)
}

// TestBranchChangesAgreeWithTheMergeAboutConflicts.
//
// The diff and the merge must apply the SAME rule. If they disagree, a reviewer
// approves a change the diff called clean and then watches the merge refuse it
// — which is the worst possible time to discover the disagreement.
func TestBranchChangesAgreeWithTheMergeAboutConflicts(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)

	clean := createTestKey(t, keys, uniqueName(t, "clean"))
	racy := createTestKey(t, keys, uniqueName(t, "racy"))
	setPortalValue(t, keys, racy.ID, "en-SG", "master v1", 0)

	branch, err := branches.Create(ctx, uniqueName(t, "diff"), "", testActor, "req-1")
	require.NoError(t, err)

	for _, k := range []int64{clean.ID, racy.ID} {
		_, err := keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
			KeyID: k, LocaleCode: "en-SG", Branch: branch.Name, Value: "branch value",
		}, testActor, "req-1")
		require.NoError(t, err)
	}

	// Master moves under one of them, AFTER the branch first touched it.
	setPortalValue(t, keys, racy.ID, "en-SG", "master v2", 1)

	_, changes, err := branches.Changes(ctx, branch.Name)
	require.NoError(t, err)
	require.Len(t, changes.Values, 2)

	byKey := map[int64]repository.BranchValueChange{}
	for _, c := range changes.Values {
		byKey[c.KeyID] = c
	}

	assert.False(t, byKey[clean.ID].Conflict, "master never moved under this pair")
	assert.False(t, byKey[clean.ID].MasterFound, "master has no row for it at all")

	conflicted := byKey[racy.ID]
	assert.True(t, conflicted.Conflict)
	assert.Equal(t, "branch value", conflicted.Value)
	assert.Equal(t, "master v2", conflicted.MasterValue, "the reviewer must see both sides")
	assert.Equal(t, 1, conflicted.BaseMasterVersion)
	assert.Equal(t, 2, conflicted.MasterVersion)

	// The merge's own conflict computation must find exactly the same row.
	conn := testGORM(t)
	mrs := repository.ProvideMergeRequestRepository(conn)
	mr, err := mrs.Create(ctx, nil, branch.ID, "diff agreement", testActor)
	require.NoError(t, err)

	fromMerge, err := mrs.Conflicts(ctx, nil, mr.ID, branch.ID)
	require.NoError(t, err)
	require.Len(t, fromMerge, 1)
	assert.Equal(t, racy.ID, fromMerge[0].KeyID)
}

// TestBranchChangesKeepTombstonesDistinctFromBlanks.
//
// A branch that removes a translation and a branch that blanks it produce very
// different merges — a DELETE against master versus an UPDATE to "". A diff
// that showed both as an empty string would give a reviewer no way to tell.
func TestBranchChangesKeepTombstonesDistinctFromBlanks(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)

	removed := createTestKey(t, keys, uniqueName(t, "removed"))
	blanked := createTestKey(t, keys, uniqueName(t, "blanked"))
	setPortalValue(t, keys, removed.ID, "en-SG", "will go away", 0)
	setPortalValue(t, keys, blanked.ID, "en-SG", "will be blank", 0)

	branch, err := branches.Create(ctx, uniqueName(t, "tomb"), "", testActor, "req-1")
	require.NoError(t, err)

	require.NoError(t, keys.DeleteTranslation(ctx, removed.ID, "en-SG", branch.Name, nil, testActor, "req-1"))
	_, err = keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: blanked.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "",
	}, testActor, "req-1")
	require.NoError(t, err)

	_, changes, err := branches.Changes(ctx, branch.Name)
	require.NoError(t, err)

	byKey := map[int64]repository.BranchValueChange{}
	for _, c := range changes.Values {
		byKey[c.KeyID] = c
	}

	assert.True(t, byKey[removed.ID].Removed, "a tombstone must be reported as a removal")
	assert.False(t, byKey[blanked.ID].Removed, "a deliberate blank is not a removal")
	assert.Equal(t, "", byKey[blanked.ID].Value)
}

// TestBranchChangesIncludeMetadataDeltas.
func TestBranchChangesIncludeMetadataDeltas(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "meta"))
	branch, err := branches.Create(ctx, uniqueName(t, "meta_br"), "", testActor, "req-1")
	require.NoError(t, err)

	_, err = keys.UpdateKey(ctx, keysvc.UpdateKeyRequest{
		KeyID: key.ID, Branch: branch.Name, Description: strPtr("renamed on the branch"),
	}, testActor, "req-1")
	require.NoError(t, err)

	_, changes, err := branches.Changes(ctx, branch.Name)
	require.NoError(t, err)
	require.Len(t, changes.Meta, 1)

	assert.Equal(t, "renamed on the branch", changes.Meta[0].Description)
	assert.Equal(t, key.Name, changes.Meta[0].MasterName)
	assert.False(t, changes.Meta[0].Conflict)
}

// TestBranchSummariesCountDeltasAndTheLiveRequest.
func TestBranchSummariesCountDeltasAndTheLiveRequest(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	branches := newBranchSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "summary"))
	branch, err := branches.Create(ctx, uniqueName(t, "summary_br"), "for counting", testActor, "req-1")
	require.NoError(t, err)

	for _, locale := range []string{"en-SG", "ms-MY"} {
		_, err := keys.SetTranslation(ctx, keysvc.SetTranslationRequest{
			KeyID: key.ID, LocaleCode: locale, Branch: branch.Name, Value: "x",
		}, testActor, "req-1")
		require.NoError(t, err)
	}

	summary, err := branches.Get(ctx, branch.Name)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.ValueChanges)
	assert.Equal(t, 0, summary.MetaChanges)
	assert.Nil(t, summary.MergeRequestID, "no request has been opened yet")

	mrs := repository.ProvideMergeRequestRepository(testGORM(t))
	mr, err := mrs.Create(ctx, nil, branch.ID, "please review", testActor)
	require.NoError(t, err)

	summary, err = branches.Get(ctx, branch.Name)
	require.NoError(t, err)
	require.NotNil(t, summary.MergeRequestID)
	assert.Equal(t, mr.ID, *summary.MergeRequestID)
	assert.Equal(t, repository.MRStatusOpen, summary.MergeRequestStatus)
}

// TestBranchLifecycleRefusals. Closing is reversible; merging is not.
func TestBranchLifecycleRefusals(t *testing.T) {
	ctx := context.Background()
	branches := newBranchSvc(t)

	name := uniqueName(t, "lifecycle")
	branch, err := branches.Create(ctx, name, "", testActor, "req-1")
	require.NoError(t, err)

	t.Run("the name is taken", func(t *testing.T) {
		_, err := branches.Create(ctx, name, "", testActor, "req-1")
		assert.ErrorIs(t, err, repository.ErrBranchNameTaken)
	})

	t.Run("close then reopen", func(t *testing.T) {
		closed, err := branches.Close(ctx, name, testActor, "req-1")
		require.NoError(t, err)
		assert.Equal(t, repository.BranchStatusClosed, closed.Status)

		// Closing an already-closed branch is not an error: a retried request
		// after a dropped response must not fail.
		_, err = branches.Close(ctx, name, testActor, "req-1")
		assert.NoError(t, err)

		reopened, err := branches.Reopen(ctx, name, testActor, "req-1")
		require.NoError(t, err)
		assert.Equal(t, repository.BranchStatusOpen, reopened.Status)
	})

	t.Run("a merged branch cannot be reopened", func(t *testing.T) {
		// A merged branch's deltas are the permanent record of what the merge
		// applied. Reopening one would invite edits to history.
		_, err := testDB.Exec(`UPDATE branches SET status = 'merged' WHERE id = $1`, branch.ID)
		require.NoError(t, err)

		_, err = branches.Reopen(ctx, name, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, branchsvc.ErrBranchNotOpen)

		_, err = branches.Close(ctx, name, testActor, "req-1")
		assert.ErrorIs(t, err, branchsvc.ErrBranchNotOpen)
	})
}

// TestBranchNameMustSurviveAURLPath.
//
// chi's {name} parameter does not match a slash, so a branch called
// "release/4.12" would be creatable and then permanently unreachable. Creation
// is the only point at which that is fixable.
func TestBranchNameMustSurviveAURLPath(t *testing.T) {
	ctx := context.Background()
	branches := newBranchSvc(t)

	for _, name := range []string{"release/4.12", "with space", "", "café"} {
		t.Run("refuses "+name, func(t *testing.T) {
			_, err := branches.Create(ctx, name, "", testActor, "req-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, branchsvc.ErrBadRequest)
		})
	}
}

// TestBranchListRejectsAnUnknownStatusFilter: silently returning everything
// shows a portal a list it did not ask for and cannot explain.
func TestBranchListRejectsAnUnknownStatusFilter(t *testing.T) {
	_, err := newBranchSvc(t).List(context.Background(), "abandoned")
	require.Error(t, err)
	assert.ErrorIs(t, err, branchsvc.ErrBadRequest)
}

// TestBranchLifecycleIsAudited.
func TestBranchLifecycleIsAudited(t *testing.T) {
	ctx := context.Background()
	branches := newBranchSvc(t)

	name := uniqueName(t, "audited")
	_, err := branches.Create(ctx, name, "", testActor, "req-1")
	require.NoError(t, err)
	_, err = branches.Close(ctx, name, testActor, "req-1")
	require.NoError(t, err)
	_, err = branches.Reopen(ctx, name, testActor, "req-1")
	require.NoError(t, err)

	rows, err := testDB.Query(
		`SELECT action FROM audit_events WHERE target = $1 ORDER BY id`, "branch:"+name)
	require.NoError(t, err)
	defer rows.Close()

	var actions []string
	for rows.Next() {
		var action string
		require.NoError(t, rows.Scan(&action))
		actions = append(actions, action)
	}
	assert.Equal(t, []string{
		repository.ActionBranchCreate, repository.ActionBranchClose, repository.ActionBranchReopen,
	}, actions)
}
