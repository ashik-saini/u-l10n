package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/lib/pq"

	"github.com/yougroupteam/u-common-components/database"
)

// Tag is one workflow label — "needs-review", "legal-approved", "q3-campaign".
//
// Tags are GLOBAL per key, not branch-scoped. That is the same call the assets
// table makes, and for the same reason: a tag is workflow metadata, not
// translatable content, so it never enters conflict computation and never joins
// the merge transaction. Every dimension kept out of the merge is a class of
// conflict nobody has to resolve.
type Tag struct {
	ID   int16
	Name string

	// Colour is carried by us because Lokalise does not expose tag colours
	// through its API — they are read off the Lokalise UI by hand at import
	// time. It is presentation-only and defaults to '', never NULL.
	Colour string

	CreatedAt time.Time
}

// TagUsage is a tag plus how many keys currently carry it.
//
// A separate type rather than a KeyCount field on Tag: the count is a property
// of the whole corpus, not of the tag, and only List can populate it. Folding
// it into Tag would mean every Tag returned by TagsForKeys carried a zero that
// reads as "no keys have this" when it actually means "nobody counted".
type TagUsage struct {
	Tag
	KeyCount int
}

// ErrTagNameTaken is returned when Create hits the tags_name_unique
// constraint.
//
// An exported sentinel rather than the driver's message, following
// ErrOptimisticLock: the portal turns this into a 409 with a sane body, and
// matching on `strings.Contains(err, "duplicate key value")` is the kind of
// coupling to a driver's phrasing that breaks on a library upgrade.
var ErrTagNameTaken = errors.New("a tag with that name already exists")

// TagRepository owns the tags and key_tags tables.
//
// Every method takes an optional tx so a service can fold tag writes into a
// larger transaction — see base.db for why a repository must never open one
// itself. Each statement below is a single statement, so each is already atomic
// on its own when tx is nil.
type TagRepository interface {
	// Create inserts a tag. A name already in use returns ErrTagNameTaken
	// rather than a raw driver error.
	Create(ctx context.Context, tx *gorm.DB, name, colour string) (Tag, error)

	// List returns every tag ordered by name, each with the number of keys
	// carrying it. The portal's tag manager shows that count, and fetching it
	// per tag would be one round trip per row for a list that is rendered whole.
	List(ctx context.Context, tx *gorm.DB) ([]TagUsage, error)

	// ByName resolves a tag by its unique name, returning ErrNotFound when
	// there is none.
	ByName(ctx context.Context, tx *gorm.DB, name string) (Tag, error)

	// ByID resolves a tag by its surrogate key. The portal addresses tags by id
	// because a rename must not break a bookmarked filter.
	ByID(ctx context.Context, tx *gorm.DB, id int16) (Tag, error)

	// ByIDs resolves many tags in one query — the bulk twin of ByID, mirroring
	// KeyRepository.IDsByName. Ids that resolve to nothing are simply absent
	// from the map, so a caller can report exactly which of a request's ids
	// were typos rather than failing on the first one it happens to try.
	ByIDs(ctx context.Context, tx *gorm.DB, ids []int16) (map[int16]Tag, error)

	// KeyExists reports whether a keys row exists.
	//
	// It lives here, not on KeyRepository, because it exists for SetKeyTags'
	// guard: key_tags.key_id references keys, and tagsvc — which owns no key
	// repository — must be able to answer 404 for a nonexistent key instead of
	// letting the FK violation surface as a 500, or worse, letting an empty
	// tag set "succeed" against a key that is not there.
	KeyExists(ctx context.Context, tx *gorm.DB, keyID int64) (bool, error)

	// Update renames or recolours a tag, returning ErrTagNameTaken when the new
	// name belongs to another tag and ErrNotFound when there is no such row.
	Update(ctx context.Context, tx *gorm.DB, id int16, name, colour string) (Tag, error)

	// KeyIDsWithTag lists the keys carrying a tag.
	//
	// It exists for Delete's audit row. key_tags has no history table and
	// tag_id cascades, so deleting a tag destroys the record that any key ever
	// carried it — reading the ids first inside the same transaction is the only
	// thing that makes the deletion reconstructable.
	KeyIDsWithTag(ctx context.Context, tx *gorm.DB, id int16) ([]int64, error)

	// Delete removes a tag, returning ErrNotFound when there was no such row.
	//
	// DESTRUCTIVE SIDE EFFECT: key_tags.tag_id is ON DELETE CASCADE, so
	// deleting a tag silently detaches it from every key that carried it. There
	// is no undo and no audit of the individual detachments — the caller is
	// expected to have confirmed this with a human first.
	Delete(ctx context.Context, tx *gorm.DB, id int16) error

	// SetKeyTags replaces the complete tag set of one key. This is what the
	// portal's tag editor calls: it sends the tags the key should end up with,
	// not a diff.
	//
	// An empty (or nil) tagIDs clears every tag on the key. That is the
	// documented way to say "no tags", not a no-op guard.
	SetKeyTags(ctx context.Context, tx *gorm.DB, keyID int64, tagIDs []int16) error

	// AddToKeys attaches one tag to many keys. Idempotent: keys that already
	// carry the tag are left exactly as they were, created_at included.
	AddToKeys(ctx context.Context, tx *gorm.DB, keyIDs []int64, tagID int16) error

	// RemoveFromKeys detaches one tag from many keys, returning how many links
	// actually went away — "detached 40 of the 50 you asked for" is a different
	// fact from "detached all 50", and the portal reports it.
	RemoveFromKeys(ctx context.Context, tx *gorm.DB, keyIDs []int64, tagID int16) (int64, error)

	// TagsForKeys returns the tags of many keys in ONE query.
	//
	// The key browser renders thousands of rows at once. A per-key lookup would
	// be thousands of round trips for a single page, which is the difference
	// between a page that loads and a page that times out. Keys with no tags
	// are simply absent from the map.
	TagsForKeys(ctx context.Context, tx *gorm.DB, keyIDs []int64) (map[int64][]Tag, error)
}

type tagRepository struct{ base }

func ProvideTagRepository(connector database.GORMConnector) TagRepository {
	return &tagRepository{base{connector: connector}}
}

const selectTagColumns = `id, name, colour, created_at`

// createTagSQL is hand-written because GORM v1 has no clause.OnConflict — that
// is a v2 API and it appears nowhere in this codebase.
//
// DO NOTHING, then no row comes back, and no row back means the name was taken.
// Detecting the duplicate through the conflict clause rather than by inspecting
// a *pq.Error keeps the check race-free and keeps the driver's wording out of
// our control flow.
const createTagSQL = `
INSERT INTO tags (name, colour)
VALUES ($1, $2)
ON CONFLICT (project_id, name) DO NOTHING
RETURNING ` + selectTagColumns

func (r *tagRepository) Create(ctx context.Context, tx *gorm.DB, name, colour string) (Tag, error) {
	var t Tag
	row := r.db(ctx, tx).Raw(createTagSQL, name, colour).Row()

	switch err := row.Scan(&t.ID, &t.Name, &t.Colour, &t.CreatedAt); {
	case err == nil:
		return t, nil
	case isNoRows(err):
		return t, fmt.Errorf("create tag %q: %w", name, ErrTagNameTaken)
	default:
		return t, fmt.Errorf("create tag %q: %w", name, err)
	}
}

// listTagsSQL counts through a LEFT JOIN so a tag nobody uses still appears,
// with a count of 0. An INNER JOIN would hide exactly the tags an operator is
// most likely to want to delete.
//
// GROUP BY on the primary key alone is legal here — Postgres derives the other
// tags columns functionally from it.
const listTagsSQL = `
SELECT t.id, t.name, t.colour, t.created_at, count(kt.key_id) AS key_count
  FROM tags t
  LEFT JOIN key_tags kt ON kt.tag_id = t.id
 GROUP BY t.id
 ORDER BY t.name`

func (r *tagRepository) List(ctx context.Context, tx *gorm.DB) ([]TagUsage, error) {
	rows, err := r.db(ctx, tx).Raw(listTagsSQL).Rows()
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()

	var out []TagUsage
	for rows.Next() {
		var u TagUsage
		if err := rows.Scan(&u.ID, &u.Name, &u.Colour, &u.CreatedAt, &u.KeyCount); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *tagRepository) ByName(ctx context.Context, tx *gorm.DB, name string) (Tag, error) {
	var t Tag
	row := r.db(ctx, tx).Raw(
		`SELECT `+selectTagColumns+` FROM tags WHERE name = $1`, name).Row()

	switch err := row.Scan(&t.ID, &t.Name, &t.Colour, &t.CreatedAt); {
	case err == nil:
		return t, nil
	case isNoRows(err):
		return t, fmt.Errorf("tag %q: %w", name, ErrNotFound)
	default:
		return t, fmt.Errorf("tag %q: %w", name, err)
	}
}

func (r *tagRepository) ByID(ctx context.Context, tx *gorm.DB, id int16) (Tag, error) {
	var t Tag
	row := r.db(ctx, tx).Raw(
		`SELECT `+selectTagColumns+` FROM tags WHERE id = $1`, id).Row()

	switch err := row.Scan(&t.ID, &t.Name, &t.Colour, &t.CreatedAt); {
	case err == nil:
		return t, nil
	case isNoRows(err):
		return t, fmt.Errorf("tag %d: %w", id, ErrNotFound)
	default:
		return t, fmt.Errorf("tag %d: %w", id, err)
	}
}

func (r *tagRepository) ByIDs(ctx context.Context, tx *gorm.DB, ids []int16) (map[int16]Tag, error) {
	out := make(map[int16]Tag, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// pq.Array over []int64 with an explicit ::smallint[] cast, the same
	// handling as SetKeyTags: lib/pq has a fast path for []int64 and Postgres
	// narrows on the cast.
	wide := make([]int64, len(ids))
	for i, id := range ids {
		wide[i] = int64(id)
	}

	rows, err := r.db(ctx, tx).Raw(
		`SELECT `+selectTagColumns+` FROM tags WHERE id = ANY($1::smallint[])`,
		pq.Array(wide)).Rows()
	if err != nil {
		return nil, fmt.Errorf("resolve %d tag ids: %w", len(ids), err)
	}
	defer rows.Close()

	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Colour, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		out[t.ID] = t
	}
	return out, rows.Err()
}

func (r *tagRepository) KeyExists(ctx context.Context, tx *gorm.DB, keyID int64) (bool, error) {
	var exists bool
	row := r.db(ctx, tx).Raw(
		`SELECT EXISTS (SELECT 1 FROM keys WHERE id = $1)`, keyID).Row()
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("check key %d exists: %w", keyID, err)
	}
	return exists, nil
}

const updateTagSQL = `
UPDATE tags SET name = $2, colour = $3
 WHERE id = $1
RETURNING ` + selectTagColumns

func (r *tagRepository) Update(
	ctx context.Context, tx *gorm.DB, id int16, name, colour string,
) (Tag, error) {
	var t Tag
	row := r.db(ctx, tx).Raw(updateTagSQL, id, name, colour).Row()

	switch err := row.Scan(&t.ID, &t.Name, &t.Colour, &t.CreatedAt); {
	case err == nil:
		return t, nil

	case isUniqueViolation(err):
		// tags_name_unique. The constraint is the authority — a pre-check would
		// be a decoration, since another tag can take the name between the check
		// and the write.
		return t, fmt.Errorf("rename tag %d to %q: %w", id, name, ErrTagNameTaken)

	case isNoRows(err):
		return t, fmt.Errorf("tag %d: %w", id, ErrNotFound)

	default:
		return t, fmt.Errorf("update tag %d: %w", id, err)
	}
}

func (r *tagRepository) KeyIDsWithTag(ctx context.Context, tx *gorm.DB, id int16) ([]int64, error) {
	rows, err := r.db(ctx, tx).Raw(
		`SELECT key_id FROM key_tags WHERE tag_id = $1 ORDER BY key_id`, id).Rows()
	if err != nil {
		return nil, fmt.Errorf("read keys carrying tag %d: %w", id, err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var keyID int64
		if err := rows.Scan(&keyID); err != nil {
			return nil, fmt.Errorf("scan tagged key: %w", err)
		}
		out = append(out, keyID)
	}
	return out, rows.Err()
}

func (r *tagRepository) Delete(ctx context.Context, tx *gorm.DB, id int16) error {
	// key_tags.tag_id cascades, so this one DELETE also detaches the tag from
	// every key carrying it. That is intended — a tag that exists only as
	// dangling links is worse than none — but it is a lot of rows to remove on
	// behalf of one click, so callers should confirm first.
	res := r.db(ctx, tx).Exec(`DELETE FROM tags WHERE id = $1`, id)
	if res.Error != nil {
		return fmt.Errorf("delete tag %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("tag %d: %w", id, ErrNotFound)
	}
	return nil
}

// setKeyTagsSQL replaces a key's tag set in ONE statement.
//
// Both halves matter and neither can be dropped:
//
//   - the data-modifying CTE removes the tags that are no longer wanted.
//     Postgres runs a data-modifying WITH clause exactly once and to
//     completion even when — as here — the primary query never reads its
//     output, which is what makes this legal rather than merely lucky.
//   - the INSERT adds the ones that are new. ON CONFLICT DO NOTHING, not
//     DO UPDATE, so a tag the key already had keeps its original created_at
//     instead of looking like it was applied just now.
//
// One statement rather than DELETE-then-INSERT because a repository must not
// open its own transaction (WithTransaction does not nest), and two statements
// outside one would leave the key momentarily untagged — visible to any
// concurrent reader, and permanent if the process dies between them.
//
// The empty case falls out for free: `tag_id <> ALL ('{}')` is true for every
// row, so an empty tagIDs deletes the lot and inserts nothing.
const setKeyTagsSQL = `
WITH desired AS (
    SELECT DISTINCT unnest($2::smallint[]) AS tag_id
), removed AS (
    DELETE FROM key_tags
     WHERE key_id = $1::bigint
       AND tag_id <> ALL ($2::smallint[])
)
INSERT INTO key_tags (key_id, tag_id)
SELECT $1::bigint, tag_id FROM desired
ON CONFLICT (key_id, tag_id) DO NOTHING`

func (r *tagRepository) SetKeyTags(ctx context.Context, tx *gorm.DB, keyID int64, tagIDs []int16) error {
	// pq.Array over []int64 with an explicit ::smallint[] cast, matching
	// UpsertBatch's handling of locale ids: lib/pq has a fast path for []int64
	// and Postgres narrows on the cast.
	ids := make([]int64, len(tagIDs))
	for i, id := range tagIDs {
		ids[i] = int64(id)
	}

	err := r.db(ctx, tx).Exec(setKeyTagsSQL, keyID, pq.Array(ids)).Error
	if err != nil {
		return fmt.Errorf("set %d tags on key %d: %w", len(tagIDs), keyID, err)
	}
	return nil
}

// addToKeysSQL fans one tag across many keys through UNNEST, the house
// bulk-write idiom (see UpsertBatch). DO NOTHING makes a re-run a no-op rather
// than a constraint violation, so a retried bulk action is safe.
const addToKeysSQL = `
INSERT INTO key_tags (key_id, tag_id)
SELECT k, $2::smallint FROM unnest($1::bigint[]) AS t(k)
ON CONFLICT (key_id, tag_id) DO NOTHING`

func (r *tagRepository) AddToKeys(ctx context.Context, tx *gorm.DB, keyIDs []int64, tagID int16) error {
	if len(keyIDs) == 0 {
		return nil
	}

	err := r.db(ctx, tx).Exec(addToKeysSQL, pq.Array(keyIDs), tagID).Error
	if err != nil {
		return fmt.Errorf("add tag %d to %d keys: %w", tagID, len(keyIDs), err)
	}
	return nil
}

func (r *tagRepository) RemoveFromKeys(
	ctx context.Context, tx *gorm.DB, keyIDs []int64, tagID int16,
) (int64, error) {
	if len(keyIDs) == 0 {
		return 0, nil
	}

	res := r.db(ctx, tx).Exec(
		`DELETE FROM key_tags WHERE tag_id = $2::smallint AND key_id = ANY($1::bigint[])`,
		pq.Array(keyIDs), tagID)
	if res.Error != nil {
		return 0, fmt.Errorf("remove tag %d from %d keys: %w", tagID, len(keyIDs), res.Error)
	}

	// Fewer rows than keys asked for is normal, not an error: the caller
	// selected a page of keys, not all of which carried the tag.
	return res.RowsAffected, nil
}

// tagsForKeysSQL resolves many keys in one round trip. Ordered by key then tag
// name so each key's slice arrives already sorted the way the browser renders
// it, which saves the caller a sort per row.
const tagsForKeysSQL = `
SELECT kt.key_id, t.id, t.name, t.colour, t.created_at
  FROM key_tags kt
  JOIN tags t ON t.id = kt.tag_id
 WHERE kt.key_id = ANY($1::bigint[])
 ORDER BY kt.key_id, t.name`

func (r *tagRepository) TagsForKeys(
	ctx context.Context, tx *gorm.DB, keyIDs []int64,
) (map[int64][]Tag, error) {
	out := make(map[int64][]Tag, len(keyIDs))
	if len(keyIDs) == 0 {
		// No query at all. An empty page is a normal outcome of a filter that
		// matched nothing, and it should cost nothing.
		return out, nil
	}

	rows, err := r.db(ctx, tx).Raw(tagsForKeysSQL, pq.Array(keyIDs)).Rows()
	if err != nil {
		return nil, fmt.Errorf("read tags for %d keys: %w", len(keyIDs), err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			keyID int64
			t     Tag
		)
		if err := rows.Scan(&keyID, &t.ID, &t.Name, &t.Colour, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan key tag: %w", err)
		}
		out[keyID] = append(out[keyID], t)
	}
	return out, rows.Err()
}
