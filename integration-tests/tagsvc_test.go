package integrationtests

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
	"github.com/yougroupteam/u-l10n/pkg/service/tagsvc"
)

// uniqueTagName is deliberately short.
//
// uniqueName embeds the test's full name, which for a nested subtest runs well
// past the 64-character limit tagsvc enforces — so a test using it would fail
// on name length rather than on the thing it is actually asserting.
func uniqueTagName(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("tag-%d-%s", atomic.AddInt64(&nameCounter, 1), suffix)
}

func newTagSvc(t *testing.T) *tagsvc.Service {
	t.Helper()
	conn := testGORM(t)
	return tagsvc.ProvideService(
		database.ProvideTransactional(conn),
		repository.ProvideTagRepository(conn),
		repository.ProvideAuditRepository(conn),
	)
}

// auditFor reads the audit rows for a target, oldest first.
func auditFor(t *testing.T, target string) []struct {
	Action   string
	Actor    string
	Metadata map[string]any
} {
	t.Helper()

	rows, err := testDB.Query(`
		SELECT action, actor, metadata::text FROM audit_events
		 WHERE target = $1 ORDER BY id`, target)
	require.NoError(t, err)
	defer rows.Close()

	var out []struct {
		Action   string
		Actor    string
		Metadata map[string]any
	}
	for rows.Next() {
		var (
			action, actor, raw string
			metadata           map[string]any
		)
		require.NoError(t, rows.Scan(&action, &actor, &raw))
		require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
		out = append(out, struct {
			Action   string
			Actor    string
			Metadata map[string]any
		}{action, actor, metadata})
	}
	require.NoError(t, rows.Err())
	return out
}

// TestEveryTagMutationIsAudited.
//
// THE reason tagsvc exists. key_tags has no history table, so these rows are the
// only trace a tag operation leaves — and "legal-approved" is a claim about
// customer-facing copy that somebody has to be answerable for.
func TestEveryTagMutationIsAudited(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	tags := newTagSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "tagged"))

	tag, err := tags.Create(ctx, uniqueTagName(t, "audit"), "#ff0000", testActor, "req-1")
	require.NoError(t, err)
	target := "tag:" + itoa(int64(tag.ID))

	_, err = tags.Update(ctx, tag.ID, uniqueTagName(t, "renamed"), "#00ff00", testActor, "req-1")
	require.NoError(t, err)
	_, err = tags.AddToKeys(ctx, tag.ID, []int64{key.ID}, testActor, "req-1")
	require.NoError(t, err)
	_, err = tags.RemoveFromKeys(ctx, tag.ID, []int64{key.ID}, testActor, "req-1")
	require.NoError(t, err)

	events := auditFor(t, target)
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
		assert.Equal(t, testActor, e.Actor)
	}
	assert.Equal(t, []string{
		repository.ActionTagCreate,
		repository.ActionTagUpdate,
		repository.ActionTagAssign,
		repository.ActionTagUnassign,
	}, actions)

	// A rename rewrites what every key carrying the tag appears to claim, so the
	// previous name must survive.
	update := events[1]
	assert.NotEmpty(t, update.Metadata["from_name"])
	assert.NotEqual(t, update.Metadata["from_name"], update.Metadata["to_name"])

	// "Detached 0 of the 1 you asked for" and "detached 1" are different facts.
	assert.Equal(t, float64(1), events[3].Metadata["removed"])
}

// TestDeletingATagRecordsWhatItDetached.
//
// The cascade is irreversible and leaves no other trace. Without the key ids in
// the audit row, "who took legal-approved off these 800 keys, and which ones?"
// has no answer at all.
func TestDeletingATagRecordsWhatItDetached(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	tags := newTagSvc(t)

	first := createTestKey(t, keys, uniqueName(t, "cascade_a"))
	second := createTestKey(t, keys, uniqueName(t, "cascade_b"))

	tag, err := tags.Create(ctx, uniqueTagName(t, "doomed"), "", testActor, "req-1")
	require.NoError(t, err)
	_, err = tags.AddToKeys(ctx, tag.ID, []int64{first.ID, second.ID}, testActor, "req-1")
	require.NoError(t, err)

	detached, err := tags.Delete(ctx, tag.ID, testActor, "req-1")
	require.NoError(t, err)
	assert.Equal(t, 2, detached)

	events := auditFor(t, "tag:"+itoa(int64(tag.ID)))
	deletion := events[len(events)-1]
	assert.Equal(t, repository.ActionTagDelete, deletion.Action)
	assert.Equal(t, float64(2), deletion.Metadata["detached_keys"])

	recorded, ok := deletion.Metadata["key_ids"].([]any)
	require.True(t, ok, "the affected key ids must be recoverable — the cascade has no undo")
	assert.Len(t, recorded, 2)

	// And the links really are gone.
	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM key_tags WHERE tag_id = $1`, tag.ID).Scan(&count))
	assert.Equal(t, 0, count)
}

// TestSetKeyTagsReplacesRatherThanMerges.
func TestSetKeyTagsReplacesRatherThanMerges(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	tags := newTagSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "replace"))

	a, err := tags.Create(ctx, uniqueTagName(t, "a"), "", testActor, "req-1")
	require.NoError(t, err)
	b, err := tags.Create(ctx, uniqueTagName(t, "b"), "", testActor, "req-1")
	require.NoError(t, err)

	_, err = tags.SetKeyTags(ctx, key.ID, []int16{a.ID, b.ID}, testActor, "req-1")
	require.NoError(t, err)

	applied, err := tags.SetKeyTags(ctx, key.ID, []int16{b.ID}, testActor, "req-1")
	require.NoError(t, err)
	require.Len(t, applied, 1)
	assert.Equal(t, b.ID, applied[0].ID)

	// An empty set is the documented way to say "no tags", not a no-op — without
	// it there would be no way to remove the last one.
	applied, err = tags.SetKeyTags(ctx, key.ID, nil, testActor, "req-1")
	require.NoError(t, err)
	assert.Empty(t, applied)

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM key_tags WHERE key_id = $1`, key.ID).Scan(&count))
	assert.Equal(t, 0, count)
}

// TestSetKeyTagsIsAllOrNothing.
//
// One bad id must not leave the key with a partially applied set: the caller
// asked for a replacement, and half a replacement is a state nobody requested.
func TestSetKeyTagsIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	tags := newTagSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "atomic"))
	good, err := tags.Create(ctx, uniqueTagName(t, "good"), "", testActor, "req-1")
	require.NoError(t, err)

	_, err = tags.SetKeyTags(ctx, key.ID, []int16{good.ID}, testActor, "req-1")
	require.NoError(t, err)

	_, err = tags.SetKeyTags(ctx, key.ID, []int16{good.ID, 32000}, testActor, "req-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, repository.ErrNotFound)

	// The original set survives untouched.
	tagsByKey, err := repository.ProvideTagRepository(testGORM(t)).
		TagsForKeys(ctx, nil, []int64{key.ID})
	require.NoError(t, err)
	require.Len(t, tagsByKey[key.ID], 1)
	assert.Equal(t, good.ID, tagsByKey[key.ID][0].ID)
}

// TestTagsSurviveTheKeyBrowser: the browser reads tags in one query for the
// whole page, and a tag applied here must show up there.
func TestTagsSurviveTheKeyBrowser(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	tags := newTagSvc(t)

	name := uniqueName(t, "browser")
	key := createTestKey(t, keys, name)

	tag, err := tags.Create(ctx, uniqueTagName(t, "filter"), "#123456", testActor, "req-1")
	require.NoError(t, err)
	_, err = tags.AddToKeys(ctx, tag.ID, []int64{key.ID}, testActor, "req-1")
	require.NoError(t, err)

	result, err := keys.Browse(ctx, keysvc.BrowseRequest{Search: name})
	require.NoError(t, err)
	require.Len(t, result.Keys, 1)
	require.Len(t, result.Keys[0].Tags, 1)
	assert.Equal(t, tag.Name, result.Keys[0].Tags[0].Name)

	// And the ?tag= filter finds it by name.
	filtered, err := keys.Browse(ctx, keysvc.BrowseRequest{Tag: tag.Name})
	require.NoError(t, err)
	require.Len(t, filtered.Keys, 1)
	assert.Equal(t, key.ID, filtered.Keys[0].Key.ID)
}

// TestTagValidationRefusesWhatWouldBeInvisiblyWrong.
func TestTagValidationRefusesWhatWouldBeInvisiblyWrong(t *testing.T) {
	ctx := context.Background()
	tags := newTagSvc(t)

	cases := []struct{ name, tagName, colour string }{
		{"empty name", "", ""},
		// A padded name looks identical to another one in every UI and collides
		// with nothing — the worst kind of duplicate.
		{"padded name", " needs-review ", ""},
		{"colour without a hash", "ok-1", "ff0000"},
		{"colour of the wrong length", "ok-2", "#fff"},
		{"colour that is not hex", "ok-3", "#gggggg"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tags.Create(ctx, tc.tagName, tc.colour, testActor, "req-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, tagsvc.ErrBadRequest)
		})
	}

	t.Run("a duplicate name is a conflict, not a fault", func(t *testing.T) {
		name := uniqueTagName(t, "dup")
		_, err := tags.Create(ctx, name, "", testActor, "req-1")
		require.NoError(t, err)

		_, err = tags.Create(ctx, name, "", testActor, "req-1")
		assert.ErrorIs(t, err, repository.ErrTagNameTaken)
	})

	t.Run("renaming onto a taken name is also a conflict", func(t *testing.T) {
		taken := uniqueTagName(t, "taken")
		_, err := tags.Create(ctx, taken, "", testActor, "req-1")
		require.NoError(t, err)

		other, err := tags.Create(ctx, uniqueTagName(t, "other"), "", testActor, "req-1")
		require.NoError(t, err)

		_, err = tags.Update(ctx, other.ID, taken, "", testActor, "req-1")
		assert.ErrorIs(t, err, repository.ErrTagNameTaken)
	})
}

// TestBulkTagRefusalsAreCallerErrors.
func TestBulkTagRefusalsAreCallerErrors(t *testing.T) {
	ctx := context.Background()
	tags := newTagSvc(t)

	tag, err := tags.Create(ctx, uniqueTagName(t, "bulk"), "", testActor, "req-1")
	require.NoError(t, err)

	_, err = tags.AddToKeys(ctx, tag.ID, nil, testActor, "req-1")
	assert.ErrorIs(t, err, tagsvc.ErrBadRequest)

	_, err = tags.AddToKeys(ctx, tag.ID, []int64{0}, testActor, "req-1")
	assert.ErrorIs(t, err, tagsvc.ErrBadRequest)

	_, err = tags.AddToKeys(ctx, 32000, []int64{1}, testActor, "req-1")
	assert.ErrorIs(t, err, repository.ErrNotFound)
}

// TestUnassignReportsWhatItActuallyRemoved.
//
// Fewer than requested is normal: the caller selected a page of keys and not all
// of them carried the tag.
func TestUnassignReportsWhatItActuallyRemoved(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	tags := newTagSvc(t)

	tagged := createTestKey(t, keys, uniqueName(t, "has_tag"))
	untagged := createTestKey(t, keys, uniqueName(t, "no_tag"))

	tag, err := tags.Create(ctx, uniqueTagName(t, "partial"), "", testActor, "req-1")
	require.NoError(t, err)
	_, err = tags.AddToKeys(ctx, tag.ID, []int64{tagged.ID}, testActor, "req-1")
	require.NoError(t, err)

	result, err := tags.RemoveFromKeys(ctx, tag.ID,
		[]int64{tagged.ID, untagged.ID}, testActor, "req-1")
	require.NoError(t, err)
	assert.Equal(t, 2, result.Requested)
	assert.Equal(t, int64(1), result.Removed)
}
