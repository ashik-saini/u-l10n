package integrationtests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
)

// These tests drive the REAL service over the REAL schema. The refusal paths
// are the point: a 409 that does not carry both values, a branch read that
// silently returns master, and a delete that writes "" instead of removing the
// row are all failures that look like success from inside a unit test with a
// stubbed repository.

const testActor = "tester@you.co"

var nameCounter int64

// uniqueName keeps tests from colliding. The package applies migrations once
// and never resets between cases, which is deliberate — it is how a test
// notices that it has accidentally become order-dependent.
func uniqueName(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("t_%s_%d_%s",
		strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")),
		atomic.AddInt64(&nameCounter, 1), suffix)
}

func newKeySvc(t *testing.T) *keysvc.Service {
	t.Helper()
	conn := testGORM(t)
	return keysvc.ProvideService(
		database.ProvideTransactional(conn),
		repository.ProvideKeyRepository(conn),
		repository.ProvideTranslationRepository(conn),
		repository.ProvideLocaleRepository(conn),
		repository.ProvideBranchRepository(conn),
		repository.ProvideTagRepository(conn),
		repository.ProvideMergeRequestRepository(conn),
		repository.ProvideAuditRepository(conn),
	)
}

func createTestKey(t *testing.T, svc *keysvc.Service, name string) model.Key {
	t.Helper()
	key, err := svc.CreateKey(context.Background(), "", keysvc.CreateKeyRequest{
		Name:      name,
		Platforms: []model.Platform{model.PlatformFlutter},
	}, testActor, "req-1")
	require.NoError(t, err)
	return key
}

func setPortalValue(t *testing.T, svc *keysvc.Service, keyID int64, locale, value string, baseVersion int) repository.Cell {
	t.Helper()
	cell, err := svc.SetTranslation(context.Background(), keysvc.SetTranslationRequest{
		KeyID: keyID, LocaleCode: locale, Value: value, BaseVersion: &baseVersion,
	}, testActor, "req-1")
	require.NoError(t, err)
	return cell
}

func createBranch(t *testing.T, name string) repository.Branch {
	t.Helper()
	branches := repository.ProvideBranchRepository(testGORM(t))
	b, err := branches.Create(context.Background(), nil, name, "", testActor)
	require.NoError(t, err)
	return b
}

// TestThreeStateSurvivesTheWholeReadPath.
//
// The one invariant everything else rests on. A key with no row, a key with a
// row holding "", and a key with text must arrive at the browser as three
// distinguishable answers — collapsing the first two adds ~430 spurious keys to
// en-SG and deletes 3,664 intentional blanks from ms-MY.
func TestThreeStateSurvivesTheWholeReadPath(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	name := uniqueName(t, "three_state")
	key := createTestKey(t, svc, name)

	setPortalValue(t, svc, key.ID, "en-SG", "Top up", 0)
	// A DELIBERATE blank. Not the same act as never translating it.
	setPortalValue(t, svc, key.ID, "ms-MY", "", 0)
	// en-MY is left alone: untranslated.

	result, err := svc.Browse(ctx, keysvc.BrowseRequest{
		Search:  name,
		Locales: []string{"en-SG", "ms-MY", "en-MY"},
	})
	require.NoError(t, err)
	require.Len(t, result.Keys, 1)

	byCode := map[string]repository.Cell{}
	for _, l := range result.Locales {
		byCode[l.Code] = result.Keys[0].Values[l.ID]
	}

	assert.True(t, byCode["en-SG"].Found)
	assert.Equal(t, "Top up", byCode["en-SG"].Value)

	assert.True(t, byCode["ms-MY"].Found, "a deliberate blank is TRANSLATED")
	assert.Equal(t, "", byCode["ms-MY"].Value)

	assert.False(t, byCode["en-MY"].Found, "no row means untranslated")

	// Every requested locale is present, including the untranslated one. An
	// absent entry would be indistinguishable from a locale nobody asked for.
	assert.Len(t, result.Keys[0].Values, 3)
}

// TestBranchReadReturnsBranchValuesNotMaster.
//
// THE test. A handler that silently returns master when asked for a branch
// shows an editor that their work did not save, and it is invisible in every
// test that does not compare the two views of the same cell.
func TestBranchReadReturnsBranchValuesNotMaster(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	name := uniqueName(t, "branch_read")
	key := createTestKey(t, svc, name)
	setPortalValue(t, svc, key.ID, "en-SG", "master value", 0)

	branch := createBranch(t, uniqueName(t, "br"))

	_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: key.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "branch value",
	}, testActor, "req-1")
	require.NoError(t, err)

	t.Run("the branch view shows the branch value", func(t *testing.T) {
		result, err := svc.Browse(ctx, keysvc.BrowseRequest{
			Search: name, Branch: branch.Name, Locales: []string{"en-SG"},
		})
		require.NoError(t, err)
		require.Len(t, result.Keys, 1)

		cell := result.Keys[0].Values[result.Locales[0].ID]
		assert.Equal(t, "branch value", cell.Value)
		assert.True(t, cell.FromBranch, "the portal marks overridden cells")
	})

	t.Run("master is untouched", func(t *testing.T) {
		result, err := svc.Browse(ctx, keysvc.BrowseRequest{
			Search: name, Locales: []string{"en-SG"},
		})
		require.NoError(t, err)
		require.Len(t, result.Keys, 1)

		cell := result.Keys[0].Values[result.Locales[0].ID]
		assert.Equal(t, "master value", cell.Value)
		assert.False(t, cell.FromBranch)
	})

	t.Run("a single-key read resolves through the branch too", func(t *testing.T) {
		// Get and Browse are different entry points, and a branch overlay
		// applied in only one of them is the kind of gap nobody notices until a
		// translator opens a key detail panel.
		result, err := svc.Get(ctx, key.ID, branch.Name, []string{"en-SG"})
		require.NoError(t, err)
		require.Len(t, result.Keys, 1)
		assert.Equal(t, "branch value", result.Keys[0].Values[result.Locales[0].ID].Value)
	})
}

// TestBranchTombstoneReadsAsUntranslatedNotBlank.
func TestBranchTombstoneReadsAsUntranslatedNotBlank(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	name := uniqueName(t, "tombstone")
	key := createTestKey(t, svc, name)
	setPortalValue(t, svc, key.ID, "en-SG", "master value", 0)

	branch := createBranch(t, uniqueName(t, "br"))
	require.NoError(t, svc.DeleteTranslation(ctx, key.ID, "en-SG", branch.Name, nil, testActor, "req-1"))

	result, err := svc.Browse(ctx, keysvc.BrowseRequest{
		Search: name, Branch: branch.Name, Locales: []string{"en-SG"},
	})
	require.NoError(t, err)
	require.Len(t, result.Keys, 1)

	cell := result.Keys[0].Values[result.Locales[0].ID]
	assert.False(t, cell.Found, "a tombstone resolves to UNTRANSLATED, not to an empty string")
	assert.Equal(t, "", cell.Value)

	// Master still has its row: a branch delta never touches master.
	master, err := svc.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.True(t, master.Keys[0].Values[master.Locales[0].ID].Found)
}

// TestOptimisticLockCarriesBothValues.
//
// A 409 whose body says only "conflict" forces the portal into a second read to
// render the dialog it must show — and that second read can return a third
// value. Both sides travel with the error.
func TestOptimisticLockCarriesBothValues(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "occ"))
	first := setPortalValue(t, svc, key.ID, "en-SG", "mine v1", 0)
	require.Equal(t, 1, first.Version)

	// Somebody else saves.
	second := setPortalValue(t, svc, key.ID, "en-SG", "theirs v2", 1)
	require.Equal(t, 2, second.Version)

	// We were still looking at version 1.
	stale := 1
	_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: key.ID, LocaleCode: "en-SG", Value: "my late edit", BaseVersion: &stale,
	}, testActor, "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrOptimisticLock,
		"a lost race must be matchable as an optimistic lock conflict")

	var conflict *keysvc.ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "my late edit", conflict.Mine)
	assert.Equal(t, "theirs v2", conflict.Theirs.Value)
	assert.True(t, conflict.Theirs.Found)
	assert.Equal(t, 2, conflict.Theirs.Version)
	assert.Equal(t, 1, conflict.ExpectedVersion)

	// And nothing was written.
	current, err := svc.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.Equal(t, "theirs v2", current.Keys[0].Values[current.Locales[0].ID].Value)
}

// TestOptimisticLockOnCreate: base_version 0 asserts "there is no row here".
// If somebody created one first, that assertion is false and the create must
// refuse rather than clobber their work.
func TestOptimisticLockOnCreate(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "occ_create"))
	setPortalValue(t, svc, key.ID, "en-SG", "somebody got there first", 0)

	base := 0
	_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: key.ID, LocaleCode: "en-SG", Value: "mine", BaseVersion: &base,
	}, testActor, "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrOptimisticLock)

	var conflict *keysvc.ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "somebody got there first", conflict.Theirs.Value)
}

// TestBaseVersionIsRequiredOnMasterAndRefusedOnABranch.
//
// The asymmetry is deliberate and it must be enforced in both directions.
// Without it on master, every save is blind last-write-wins. Accepting it on a
// branch and ignoring it would be worse than refusing it: the portal would
// believe it had concurrency control that does not exist.
func TestBaseVersionIsRequiredOnMasterAndRefusedOnABranch(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "base_version"))
	branch := createBranch(t, uniqueName(t, "br"))

	t.Run("missing on master", func(t *testing.T) {
		_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
			KeyID: key.ID, LocaleCode: "en-SG", Value: "x",
		}, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, keysvc.ErrBadRequest)
		assert.Contains(t, err.Error(), "base_version is required")
	})

	t.Run("present on a branch", func(t *testing.T) {
		base := 0
		_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
			KeyID: key.ID, LocaleCode: "en-SG", Branch: branch.Name,
			Value: "x", BaseVersion: &base,
		}, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, keysvc.ErrBadRequest)
	})
}

// TestDeleteTranslationRemovesTheRowRatherThanBlankingIt.
//
// The distinction that ~430 untranslated en-SG keys depend on. A delete that
// wrote "" would quietly convert every one of them into a deliberate blank.
func TestDeleteTranslationRemovesTheRowRatherThanBlankingIt(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "delete_value"))
	setPortalValue(t, svc, key.ID, "en-SG", "goodbye", 0)

	require.NoError(t, svc.DeleteTranslation(ctx, key.ID, "en-SG", "", nil, testActor, "req-1"))

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM translations WHERE key_id = $1`, key.ID).Scan(&count))
	assert.Equal(t, 0, count, "the ROW must be gone, not blanked")

	result, err := svc.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.False(t, result.Keys[0].Values[result.Locales[0].ID].Found)

	// Deleting again is a 404, not a silent success: "removed it" and "there was
	// nothing to remove" are different answers.
	err = svc.DeleteTranslation(ctx, key.ID, "en-SG", "", nil, testActor, "req-1")
	assert.ErrorIs(t, err, repository.ErrNotFound)
}

// TestDeleteTranslationRecordsBecameUntranslated: the history row must carry
// NULL, not "", or the timeline loses the same distinction the table does.
func TestDeleteTranslationRecordsBecameUntranslated(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "history_null"))
	setPortalValue(t, svc, key.ID, "en-SG", "before", 0)
	require.NoError(t, svc.DeleteTranslation(ctx, key.ID, "en-SG", "", nil, testActor, "req-1"))

	_, values, err := svc.History(ctx, key.ID, "en-SG", 10)
	require.NoError(t, err)
	require.Len(t, values, 2, "one row for the write, one for the removal")

	// Newest first.
	assert.Nil(t, values[0].Value, "a removal is recorded as NULL, not as an empty string")
	require.NotNil(t, values[1].Value)
	assert.Equal(t, "before", *values[1].Value)
}

// TestUntranslatedInResolvesThroughTheBranch.
//
// The "what is left to do" filter is the one most likely to be written against
// master by accident, and the failure is silent: it lists keys the translator
// already filled in on their branch.
func TestUntranslatedInResolvesThroughTheBranch(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	prefix := uniqueName(t, "untranslated")
	done := createTestKey(t, svc, prefix+"_done")
	todo := createTestKey(t, svc, prefix+"_todo")

	branch := createBranch(t, uniqueName(t, "br"))
	_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: done.ID, LocaleCode: "th-TH", Branch: branch.Name, Value: "แปลแล้ว",
	}, testActor, "req-1")
	require.NoError(t, err)

	t.Run("on master both are outstanding", func(t *testing.T) {
		result, err := svc.Browse(ctx, keysvc.BrowseRequest{
			Search: prefix, UntranslatedIn: "th-TH",
		})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 2)
	})

	t.Run("on the branch only one is", func(t *testing.T) {
		result, err := svc.Browse(ctx, keysvc.BrowseRequest{
			Search: prefix, Branch: branch.Name, UntranslatedIn: "th-TH",
		})
		require.NoError(t, err)
		require.Len(t, result.Keys, 1)
		assert.Equal(t, todo.Name, result.Keys[0].Key.Name)
	})

	t.Run("a deliberate blank is translated", func(t *testing.T) {
		// The filter must not treat "" as outstanding work: somebody decided
		// that string is intentionally empty.
		setPortalValue(t, svc, todo.ID, "th-TH", "", 0)

		result, err := svc.Browse(ctx, keysvc.BrowseRequest{
			Search: prefix, UntranslatedIn: "th-TH",
		})
		require.NoError(t, err)
		require.Len(t, result.Keys, 1)
		assert.Equal(t, done.Name, result.Keys[0].Key.Name)
	})
}

// TestBrowseRejectsWhatItCannotHonour. Silently returning a subset is how a
// caller concludes a locale is untranslated everywhere.
func TestBrowseRejectsWhatItCannotHonour(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	cases := []struct {
		name string
		req  keysvc.BrowseRequest
	}{
		{"unknown locale", keysvc.BrowseRequest{Locales: []string{"en-SG", "fr-FR"}}},
		{"unknown untranslated_in", keysvc.BrowseRequest{UntranslatedIn: "fr-FR"}},
		{"unknown platform", keysvc.BrowseRequest{Platform: "windows"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Browse(ctx, tc.req)
			require.Error(t, err)
			assert.ErrorIs(t, err, keysvc.ErrBadRequest)
		})
	}

	t.Run("unknown branch is a 404, not a bad request", func(t *testing.T) {
		// The caller's syntax is fine; the branch simply is not there. Reporting
		// it as a bad request would send them looking for a typo in their query.
		_, err := svc.Browse(ctx, keysvc.BrowseRequest{Branch: "no-such-branch"})
		require.Error(t, err)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

// TestKeyLifecycleOnMaster covers create, rename, soft delete and name reuse.
func TestKeyLifecycleOnMaster(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	name := uniqueName(t, "lifecycle")
	key := createTestKey(t, svc, name)

	t.Run("the name is reserved while active", func(t *testing.T) {
		_, err := svc.CreateKey(ctx, "", keysvc.CreateKeyRequest{
			Name: name, Platforms: []model.Platform{model.PlatformFlutter},
		}, testActor, "req-1")
		assert.ErrorIs(t, err, repository.ErrKeyNameTaken)
	})

	t.Run("a stale base_version refuses the update", func(t *testing.T) {
		stale := key.Version + 99
		_, err := svc.UpdateKey(ctx, keysvc.UpdateKeyRequest{
			KeyID: key.ID, Description: strPtr("nope"), BaseVersion: &stale,
		}, testActor, "req-1")
		assert.ErrorIs(t, err, repository.ErrOptimisticLock)
	})

	t.Run("android_name round-trips through null", func(t *testing.T) {
		// NULL means "derive from the key name" and is a real instruction, not
		// an absence. Setting it and clearing it must both land.
		set, err := svc.UpdateKey(ctx, keysvc.UpdateKeyRequest{
			KeyID: key.ID, AndroidName: strPtr("custom_android_name"),
		}, testActor, "req-1")
		require.NoError(t, err)
		require.NotNil(t, set.AndroidName)
		assert.Equal(t, "custom_android_name", *set.AndroidName)

		cleared, err := svc.UpdateKey(ctx, keysvc.UpdateKeyRequest{
			KeyID: key.ID, ClearAndroidName: true,
		}, testActor, "req-1")
		require.NoError(t, err)
		assert.Nil(t, cleared.AndroidName, "clearing must reach NULL, not an empty string")
	})

	t.Run("soft delete frees the name", func(t *testing.T) {
		_, err := svc.DeleteKey(ctx, key.ID, "", nil, testActor, "req-1")
		require.NoError(t, err)

		// The row survives — history references it — but idx_keys_name_active is
		// partial, so the name is available again.
		var status string
		require.NoError(t, testDB.QueryRow(
			`SELECT status FROM keys WHERE id = $1`, key.ID).Scan(&status))
		assert.Equal(t, "deleted", status)

		reused, err := svc.CreateKey(ctx, "", keysvc.CreateKeyRequest{
			Name: name, Platforms: []model.Platform{model.PlatformFlutter},
		}, testActor, "req-1")
		require.NoError(t, err)
		assert.NotEqual(t, key.ID, reused.ID)
	})
}

// TestCreateKeyOnABranchIsADraftPlusADelta.
//
// The shape of the whole fix. A key created on a branch is a REAL keys row from
// the moment it is created — held at status 'draft' — plus an ordinary
// branch_keys delta saying 'active'. That is what makes it fillable
// (branch_translations.key_id is NOT NULL REFERENCES keys) while keeping it out
// of every export until a merge promotes it.
func TestCreateKeyOnABranchIsADraftPlusADelta(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)
	branch := createBranch(t, uniqueName(t, "br"))

	name := uniqueName(t, "on_branch")
	created, err := svc.CreateKey(ctx, branch.Name, keysvc.CreateKeyRequest{
		Name: name, Platforms: []model.Platform{model.PlatformFlutter},
	}, testActor, "req-1")
	require.NoError(t, err)
	require.NotZero(t, created.ID)

	// Master holds a draft. Anything else and the key would be exported before
	// anybody approved it.
	var masterStatus string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM keys WHERE id = $1`, created.ID).Scan(&masterStatus))
	assert.Equal(t, "draft", masterStatus,
		"a key created on a branch must not be active on master before the merge")

	// The delta carries the key's id — never NULL — and the instruction the
	// merge will carry out.
	var (
		deltaKeyID sql.NullInt64
		deltaState string
		baseVer    int
	)
	require.NoError(t, testDB.QueryRow(`
		SELECT key_id, status, base_master_version
		  FROM branch_keys WHERE branch_id = $1 AND name = $2`,
		branch.ID, name).Scan(&deltaKeyID, &deltaState, &baseVer))
	require.True(t, deltaKeyID.Valid, "branch_keys.key_id must name the draft row")
	assert.Equal(t, created.ID, deltaKeyID.Int64)
	assert.Equal(t, "active", deltaState, "the delta is the promotion the merge applies")
	assert.Equal(t, 1, baseVer, "anchored on the draft's own keys.version")

	// The point of the draft: values can be attached, because the foreign key
	// now has something to point at.
	_, err = svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: created.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "brand new copy",
	}, testActor, "req-1")
	require.NoError(t, err)

	t.Run("invisible on master, visible on its own branch", func(t *testing.T) {
		onMaster, err := svc.Browse(ctx, keysvc.BrowseRequest{Search: name})
		require.NoError(t, err)
		assert.Empty(t, onMaster.Keys, "a draft is not part of master's corpus")

		onBranch, err := svc.Browse(ctx, keysvc.BrowseRequest{Branch: branch.Name, Search: name})
		require.NoError(t, err)
		require.Len(t, onBranch.Keys, 1,
			"the branch's own browser must show the key the branch just created")
		assert.Equal(t, model.KeyStatusActive, onBranch.Keys[0].Key.Status,
			"the branch overlay reports the status the branch intends")
	})

	t.Run("the branch diff shows it as an introduction", func(t *testing.T) {
		_, changes, err := newBranchSvc(t).Changes(ctx, branch.Name)
		require.NoError(t, err)

		var found bool
		for _, m := range changes.Meta {
			if m.KeyID != created.ID {
				continue
			}
			found = true
			assert.Equal(t, "draft", m.MasterStatus,
				"master_status 'draft' is how a reviewer tells a new key from a rename")
			assert.Equal(t, "active", m.Status)
			assert.False(t, m.Conflict)
		}
		assert.True(t, found, "the diff must carry the created key")
	})

	t.Run("the name is still checked against active master keys", func(t *testing.T) {
		taken := uniqueName(t, "already_live")
		createTestKey(t, svc, taken)

		_, err := svc.CreateKey(ctx, branch.Name, keysvc.CreateKeyRequest{
			Name: taken, Platforms: []model.Platform{model.PlatformFlutter},
		}, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, repository.ErrKeyNameTaken)
	})

	t.Run("a closed branch cannot gain keys", func(t *testing.T) {
		branches := newBranchSvc(t)
		_, err := branches.Close(ctx, branch.Name, testActor, "req-1")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, reopenErr := branches.Reopen(ctx, branch.Name, testActor, "req-1")
			require.NoError(t, reopenErr)
		})

		_, err = svc.CreateKey(ctx, branch.Name, keysvc.CreateKeyRequest{
			Name: uniqueName(t, "too_late"), Platforms: []model.Platform{model.PlatformFlutter},
		}, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, keysvc.ErrBranchNotOpen)
	})
}

// TestBranchKeyMetadataOverlaysTheBrowser: a rename on a branch must be visible
// in the branch's own view and invisible on master.
func TestBranchKeyMetadataOverlaysTheBrowser(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	name := uniqueName(t, "meta_overlay")
	key := createTestKey(t, svc, name)
	branch := createBranch(t, uniqueName(t, "br"))

	_, err := svc.UpdateKey(ctx, keysvc.UpdateKeyRequest{
		KeyID: key.ID, Branch: branch.Name, Description: strPtr("branch-only note"),
	}, testActor, "req-1")
	require.NoError(t, err)

	onBranch, err := svc.Get(ctx, key.ID, branch.Name, []string{"en-SG"})
	require.NoError(t, err)
	assert.Equal(t, "branch-only note", onBranch.Keys[0].Key.Description)
	assert.True(t, onBranch.Keys[0].BranchModified)

	onMaster, err := svc.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.Equal(t, "", onMaster.Keys[0].Key.Description)
	assert.False(t, onMaster.Keys[0].BranchModified)
}

// TestWritesToAClosedBranchAreRefused. A merged branch's deltas are the
// permanent record of what a merge applied; editing them would edit history.
func TestWritesToAClosedBranchAreRefused(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "closed_branch"))
	branch := createBranch(t, uniqueName(t, "br"))

	branches := repository.ProvideBranchRepository(testGORM(t))
	require.NoError(t, branches.SetStatus(ctx, nil, branch.ID, repository.BranchStatusClosed))

	_, err := svc.SetTranslation(ctx, keysvc.SetTranslationRequest{
		KeyID: key.ID, LocaleCode: "en-SG", Branch: branch.Name, Value: "x",
	}, testActor, "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, keysvc.ErrBranchNotOpen)
}

// TestKeyValidationRefusesWhatTheSchemaWouldRejectLater.
//
// The constraints would catch these too, but a constraint violation surfaces as
// a 500 with a Postgres string in it, and every one of these is a typo.
func TestKeyValidationRefusesWhatTheSchemaWouldRejectLater(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	cases := []struct {
		name string
		req  keysvc.CreateKeyRequest
	}{
		{"no name", keysvc.CreateKeyRequest{Platforms: []model.Platform{model.PlatformFlutter}}},
		{"whitespace around the name", keysvc.CreateKeyRequest{
			Name: " padded ", Platforms: []model.Platform{model.PlatformFlutter}}},
		{"no platforms", keysvc.CreateKeyRequest{Name: uniqueName(t, "np")}},
		{"unknown platform", keysvc.CreateKeyRequest{
			Name: uniqueName(t, "up"), Platforms: []model.Platform{"windows"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateKey(ctx, "", tc.req, testActor, "req-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, keysvc.ErrBadRequest)
			assert.False(t, errors.Is(err, repository.ErrNotFound))
		})
	}
}

// TestKeyMutationsAreAudited. audit_events answers "who did this" for the
// structural changes that key_history alone cannot express.
func TestKeyMutationsAreAudited(t *testing.T) {
	ctx := context.Background()
	svc := newKeySvc(t)

	key := createTestKey(t, svc, uniqueName(t, "audited"))
	_, err := svc.UpdateKey(ctx, keysvc.UpdateKeyRequest{
		KeyID: key.ID, Description: strPtr("note"),
	}, testActor, "req-1")
	require.NoError(t, err)
	_, err = svc.DeleteKey(ctx, key.ID, "", nil, testActor, "req-1")
	require.NoError(t, err)

	rows, err := testDB.Query(
		`SELECT action FROM audit_events WHERE target = $1 ORDER BY id`,
		fmt.Sprintf("key:%d", key.ID))
	require.NoError(t, err)
	defer rows.Close()

	var actions []string
	for rows.Next() {
		var action string
		require.NoError(t, rows.Scan(&action))
		actions = append(actions, action)
	}
	assert.Equal(t, []string{
		repository.ActionKeyCreate, repository.ActionKeyUpdate, repository.ActionKeyDelete,
	}, actions)
}

func strPtr(s string) *string { return &s }
