package integrationtests

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// These exercise pkg/repository/tag.go itself rather than a copy of its SQL.
//
// asset_repo_test.go mirrors the statements by hand, which proves the SQL is
// valid but not that the repository issues it — a typo in tag.go would leave
// those tests green. Driving the real TagRepository closes that gap, and costs
// only the small adapter below, because GORMConnector is two methods and GORM
// v1 will open on an existing *sql.DB.
//
// What is worth proving here is the behaviour the hand-written SQL encodes and
// no compiler checks: that SetKeyTags REPLACES, that the bulk writes are
// idempotent, and that deleting a tag really does take its links with it.

// testConnector adapts the suite's *sql.DB to the GORMConnector the
// repositories expect. Context is dropped deliberately: the real connector only
// uses it to attach APM spans, and there is no APM here.
type testConnector struct{ db *gorm.DB }

func (c testConnector) GetDB() *gorm.DB                             { return c.db }
func (c testConnector) GetDBWithContext(_ context.Context) *gorm.DB { return c.db }

var (
	tagRepoOnce sync.Once
	tagRepoInst repository.TagRepository
	tagRepoErr  error
)

// tagRepo builds the repository once and hands it to every test. nil is passed
// as the tx throughout: these cases are about the statements, and the optional
// tx is the caller's business.
func tagRepo(t *testing.T) repository.TagRepository {
	t.Helper()
	tagRepoOnce.Do(func() {
		var db *gorm.DB
		db, tagRepoErr = gorm.Open("postgres", testDB)
		if tagRepoErr == nil {
			tagRepoInst = repository.ProvideTagRepository(testConnector{db: db})
		}
	})
	require.NoError(t, tagRepoErr, "open gorm over the test database")
	return tagRepoInst
}

// tagIDsOf reads a key's tags straight from the table, bypassing the
// repository. An assertion that read back through the same code under test
// could only prove the code agrees with itself.
func tagIDsOf(t *testing.T, keyID int64) []int16 {
	t.Helper()
	rows, err := testDB.Query(
		`SELECT tag_id FROM key_tags WHERE key_id = $1 ORDER BY tag_id`, keyID)
	require.NoError(t, err)
	defer rows.Close()

	out := []int16{}
	for rows.Next() {
		var id int16
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// findTag locates one tag in a List result. The suite shares a database across
// tests and never resets it, so a test must never assert on the length of a
// global listing — an unrelated new test would break it.
func findTag(t *testing.T, list []repository.TagUsage, name string) repository.TagUsage {
	t.Helper()
	for _, u := range list {
		if u.Name == name {
			return u
		}
	}
	t.Fatalf("tag %q missing from List", name)
	return repository.TagUsage{}
}

func TestTagCreateRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)

	created, err := repo.Create(ctx, nil, "tag.dup", "#ff0000")
	require.NoError(t, err)
	assert.NotZero(t, created.ID)
	assert.Equal(t, "#ff0000", created.Colour,
		"colour must survive the round trip: Lokalise does not serve it, so a lost "+
			"colour cannot be re-imported and has to be read off the UI again by hand")

	// No constraint name to assert: createTagSQL is ON CONFLICT DO NOTHING and
	// the repository reads "no row returned" as the duplicate, so
	// tags_project_name_unique never raises a driver error at all. The
	// sentinel assertion below is what carries the discrimination here.
	_, err = repo.Create(ctx, nil, "tag.dup", "#00ff00")
	requireRejectedBySentinel(t, err, "a second tag named tag.dup")

	// A typed sentinel, so the handler can answer 409 without matching on the
	// driver's wording.
	assert.True(t, errors.Is(err, repository.ErrTagNameTaken),
		"want ErrTagNameTaken, got %v", err)
	assert.NotContains(t, err.Error(), "SQLSTATE",
		"the driver's message must not leak to the caller")

	// The loser must not have changed the winner.
	existing, err := repo.ByName(ctx, nil, "tag.dup")
	require.NoError(t, err)
	assert.Equal(t, "#ff0000", existing.Colour)
}

func TestTagByNameReportsAbsence(t *testing.T) {
	_, err := tagRepo(t).ByName(context.Background(), nil, "tag.never.created")
	assert.True(t, errors.Is(err, repository.ErrNotFound), "want ErrNotFound, got %v", err)
}

// TestSetKeyTagsReplacesRatherThanAppends is the case a naive implementation
// gets wrong: an INSERT ... ON CONFLICT DO NOTHING alone would leave the tags
// the editor removed still attached, and the portal would appear to ignore
// every deselection.
func TestSetKeyTagsReplacesRatherThanAppends(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)
	keyID := insertKey(t, "tag.setkeytags.replace")

	a, err := repo.Create(ctx, nil, "tag.replace.a", "")
	require.NoError(t, err)
	b, err := repo.Create(ctx, nil, "tag.replace.b", "")
	require.NoError(t, err)
	c, err := repo.Create(ctx, nil, "tag.replace.c", "")
	require.NoError(t, err)

	require.NoError(t, repo.SetKeyTags(ctx, nil, keyID, []int16{a.ID, b.ID}))
	assert.ElementsMatch(t, []int16{a.ID, b.ID}, tagIDsOf(t, keyID))

	var bAttachedAt string
	require.NoError(t, testDB.QueryRow(
		`SELECT created_at FROM key_tags WHERE key_id = $1 AND tag_id = $2`,
		keyID, b.ID).Scan(&bAttachedAt))

	// b survives, a goes, c arrives — all from one call that names only the
	// desired end state.
	require.NoError(t, repo.SetKeyTags(ctx, nil, keyID, []int16{b.ID, c.ID}))
	assert.ElementsMatch(t, []int16{b.ID, c.ID}, tagIDsOf(t, keyID),
		"SetKeyTags must replace the set, not add to it")

	// A tag that was already there must not be re-stamped. DO NOTHING rather
	// than DO UPDATE is what keeps "tagged since Tuesday" true.
	var bStillAt string
	require.NoError(t, testDB.QueryRow(
		`SELECT created_at FROM key_tags WHERE key_id = $1 AND tag_id = $2`,
		keyID, b.ID).Scan(&bStillAt))
	assert.Equal(t, bAttachedAt, bStillAt, "an unchanged link must keep its created_at")

	// Repeating the same call changes nothing.
	require.NoError(t, repo.SetKeyTags(ctx, nil, keyID, []int16{b.ID, c.ID}))
	assert.ElementsMatch(t, []int16{b.ID, c.ID}, tagIDsOf(t, keyID))

	// Duplicates in the input are the caller's sloppiness, not an error: the
	// composite primary key would reject the second copy without the DISTINCT.
	require.NoError(t, repo.SetKeyTags(ctx, nil, keyID, []int16{c.ID, c.ID, b.ID}))
	assert.ElementsMatch(t, []int16{b.ID, c.ID}, tagIDsOf(t, keyID))
}

// TestSetKeyTagsWithEmptySliceClearsEverything: "no tags" is a legitimate end
// state the editor can request, not an input to guard against.
func TestSetKeyTagsWithEmptySliceClearsEverything(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)
	keyID := insertKey(t, "tag.setkeytags.clear")

	tag, err := repo.Create(ctx, nil, "tag.clear.one", "")
	require.NoError(t, err)
	other, err := repo.Create(ctx, nil, "tag.clear.two", "")
	require.NoError(t, err)

	// A second key carrying the same tags, to prove the clear is scoped to one.
	bystander := insertKey(t, "tag.setkeytags.bystander")
	require.NoError(t, repo.SetKeyTags(ctx, nil, bystander, []int16{tag.ID, other.ID}))

	require.NoError(t, repo.SetKeyTags(ctx, nil, keyID, []int16{tag.ID, other.ID}))
	require.Len(t, tagIDsOf(t, keyID), 2)

	require.NoError(t, repo.SetKeyTags(ctx, nil, keyID, nil))
	assert.Empty(t, tagIDsOf(t, keyID), "an empty set must clear the key")
	assert.Len(t, tagIDsOf(t, bystander), 2, "clearing one key must not touch another")

	// The tags themselves are untouched — detaching is not deleting.
	_, err = repo.ByName(ctx, nil, "tag.clear.one")
	assert.NoError(t, err)
}

// TestTagAddToKeysIsIdempotent: bulk actions get retried after a dropped
// response, and a retry must not be a constraint violation.
func TestTagAddToKeysIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)

	tag, err := repo.Create(ctx, nil, "tag.bulk.add", "")
	require.NoError(t, err)

	first := insertKey(t, "tag.bulk.add.1")
	second := insertKey(t, "tag.bulk.add.2")
	keyIDs := []int64{first, second}

	require.NoError(t, repo.AddToKeys(ctx, nil, keyIDs, tag.ID))
	require.NoError(t, repo.AddToKeys(ctx, nil, keyIDs, tag.ID))

	var links int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM key_tags WHERE tag_id = $1`, tag.ID).Scan(&links))
	assert.Equal(t, 2, links, "running the bulk add twice must leave one row per key")

	// An empty selection is a no-op, not an error and not a query.
	assert.NoError(t, repo.AddToKeys(ctx, nil, nil, tag.ID))

	// RemoveFromKeys reports what it actually did. Only one of these two keys
	// carries the tag by the time the third id is thrown in, so the count is
	// the useful answer rather than len(keyIDs).
	removed, err := repo.RemoveFromKeys(ctx, nil, []int64{first}, tag.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	removed, err = repo.RemoveFromKeys(ctx, nil, keyIDs, tag.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed, "only the still-attached key counts")

	removed, err = repo.RemoveFromKeys(ctx, nil, nil, tag.ID)
	require.NoError(t, err)
	assert.Zero(t, removed)
}

// TestTagDeleteCascadesToKeyTags documents the destructive side effect in a
// place that fails if it ever stops being true: deleting a tag detaches it
// everywhere, with no per-key record of what was undone.
func TestTagDeleteCascadesToKeyTags(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)

	tag, err := repo.Create(ctx, nil, "tag.delete.cascade", "")
	require.NoError(t, err)
	keyID := insertKey(t, "tag.delete.cascade.key")
	require.NoError(t, repo.AddToKeys(ctx, nil, []int64{keyID}, tag.ID))
	require.Len(t, tagIDsOf(t, keyID), 1)

	require.NoError(t, repo.Delete(ctx, nil, tag.ID))

	assert.Empty(t, tagIDsOf(t, keyID), "deleting a tag must take its key_tags rows with it")

	// The key itself survives: a tag is metadata about a key, never the other
	// way round.
	var keyAlive bool
	require.NoError(t, testDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM keys WHERE id = $1)`, keyID).Scan(&keyAlive))
	assert.True(t, keyAlive)

	// A second delete removed nothing, and says so.
	err = repo.Delete(ctx, nil, tag.ID)
	assert.True(t, errors.Is(err, repository.ErrNotFound), "want ErrNotFound, got %v", err)
}

// TestTagsForKeysGroupsInOneCall covers the key browser's actual access
// pattern: many keys, mixed tags, one query.
func TestTagsForKeysGroupsInOneCall(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)

	red, err := repo.Create(ctx, nil, "tag.forkeys.red", "#ff0000")
	require.NoError(t, err)
	blue, err := repo.Create(ctx, nil, "tag.forkeys.blue", "#0000ff")
	require.NoError(t, err)

	both := insertKey(t, "tag.forkeys.both")
	oneOnly := insertKey(t, "tag.forkeys.one")
	none := insertKey(t, "tag.forkeys.none")

	require.NoError(t, repo.SetKeyTags(ctx, nil, both, []int16{red.ID, blue.ID}))
	require.NoError(t, repo.SetKeyTags(ctx, nil, oneOnly, []int16{blue.ID}))

	got, err := repo.TagsForKeys(ctx, nil, []int64{both, oneOnly, none})
	require.NoError(t, err)

	// An untagged key is absent rather than present-and-empty, so a caller can
	// range over the map without checking for empty slices.
	require.Len(t, got, 2)
	assert.NotContains(t, got, none)

	require.Len(t, got[both], 2)
	// Ordered by tag name, so the caller renders without sorting: blue < red.
	assert.Equal(t, "tag.forkeys.blue", got[both][0].Name)
	assert.Equal(t, "tag.forkeys.red", got[both][1].Name)
	assert.Equal(t, "#ff0000", got[both][1].Colour)

	require.Len(t, got[oneOnly], 1)
	assert.Equal(t, "tag.forkeys.blue", got[oneOnly][0].Name)

	// No keys means no query and no error — an empty page is the normal result
	// of a filter that matched nothing.
	empty, err := repo.TagsForKeys(ctx, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestTagListCountsKeys: the count is what the portal shows next to each tag,
// and it has to track both directions.
func TestTagListCountsKeys(t *testing.T) {
	ctx := context.Background()
	repo := tagRepo(t)

	counted, err := repo.Create(ctx, nil, "tag.list.counted", "#123456")
	require.NoError(t, err)
	unused, err := repo.Create(ctx, nil, "tag.list.unused", "")
	require.NoError(t, err)

	// A tag nobody uses must still be listed, at zero. An INNER JOIN would drop
	// exactly the tags an operator wants to find and delete.
	before := findTag(t, mustList(t, repo), "tag.list.unused")
	assert.Equal(t, 0, before.KeyCount)
	assert.Equal(t, unused.ID, before.ID)

	first := insertKey(t, "tag.list.count.1")
	second := insertKey(t, "tag.list.count.2")
	require.NoError(t, repo.AddToKeys(ctx, nil, []int64{first, second}, counted.ID))

	attached := findTag(t, mustList(t, repo), "tag.list.counted")
	assert.Equal(t, 2, attached.KeyCount)
	assert.Equal(t, "#123456", attached.Colour)

	removed, err := repo.RemoveFromKeys(ctx, nil, []int64{first}, counted.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)

	detached := findTag(t, mustList(t, repo), "tag.list.counted")
	assert.Equal(t, 1, detached.KeyCount, "the count must fall when a tag is detached")

	// Ordering is by name and must hold across the whole listing.
	list := mustList(t, repo)
	for i := 1; i < len(list); i++ {
		assert.LessOrEqual(t, list[i-1].Name, list[i].Name, "List must be ordered by name")
	}
}

func mustList(t *testing.T, repo repository.TagRepository) []repository.TagUsage {
	t.Helper()
	list, err := repo.List(context.Background(), nil)
	require.NoError(t, err)
	return list
}
