package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/lib/pq"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// ErrKeyNameTaken is returned when a create or a rename would collide with an
// ACTIVE key of the same name.
//
// An exported sentinel rather than a driver error string, following
// ErrTagNameTaken: the portal turns this into a 409, and matching on
// `strings.Contains(err, "duplicate key value")` is coupling to a phrasing that
// changes on a library upgrade. Note the collision is only against active keys —
// idx_keys_name_active is partial, so a soft-deleted key does not reserve its
// name forever.
var ErrKeyNameTaken = errors.New("an active key with that name already exists")

// KeyFilter narrows a key-browser query.
//
// The zero value means "every active key on master, unpaged". Each field is
// additive: they AND together, because that is what the portal's filter bar
// does.
type KeyFilter struct {
	// BranchID selects the copy-on-write view. Zero means master — and because
	// branches.id is a BIGSERIAL starting at 1, zero simply matches no delta
	// row, so master and branch reads take ONE code path rather than two that
	// drift apart. Two things here consult it — the status predicate, so a key
	// the branch created is visible in the branch's own view while still a draft
	// on master, and UntranslatedIn. The values themselves are resolved by
	// TranslationRepository.ResolveMany.
	BranchID int64

	// Platform restricts to keys that ship on one platform.
	Platform string

	// Tag restricts to keys carrying a tag, by tag NAME. Names are what the
	// portal's filter bar has; ids are an implementation detail of this schema.
	Tag string

	// Search is a case-insensitive substring match over the key name, its
	// description, and its translated values in ANY locale. Values are included
	// because a copywriter looking for "Top up" knows the English, not the key.
	Search string

	// UntranslatedIn keeps only keys with no value for this locale, resolved
	// through BranchID. Zero means no filter. This is the "what is left to do"
	// view, and it is the one filter that MUST respect the three-state rule: a
	// key whose value is a deliberate "" is translated and must not appear.
	UntranslatedIn int16

	// IncludeDeleted widens the status filter to soft-deleted and draft keys.
	IncludeDeleted bool

	// Limit and Offset page the result. A zero Limit means unlimited — the key
	// browser genuinely fetches the whole corpus in one request, and a silent
	// default would truncate it.
	Limit  int
	Offset int
}

// KeyPage is one page of keys plus the size of the whole matching set.
//
// Total comes from a window function in the same query rather than a second
// COUNT: two queries could disagree, and a portal that shows "1-100 of 6,300"
// while page 63 is empty is showing a lie.
type KeyPage struct {
	Keys  []model.Key
	Total int
}

// KeyHistoryEntry is one row of the append-only key_history table.
type KeyHistoryEntry struct {
	ID          int64
	KeyID       int64
	Name        string
	Description string
	Platforms   []model.Platform
	Status      string
	Version     int
	Source      string
	BranchID    *int64
	ChangedBy   string
	ChangedAt   time.Time
}

// KeyRepository owns the keys table.
type KeyRepository interface {
	// UpsertByName creates or updates a key identified by its canonical name,
	// returning the row id. Idempotent, so an interrupted import can simply be
	// re-run.
	UpsertByName(ctx context.Context, tx *gorm.DB, k model.Key) (int64, error)

	// IDsByName resolves many names in one query, which keeps the importer from
	// issuing 6,000 round trips.
	IDsByName(ctx context.Context, tx *gorm.DB, names []string) (map[string]int64, error)

	// MaxSortIndex reports the highest sort_index in use, so new keys can be
	// appended after existing ones rather than renumbering.
	MaxSortIndex(ctx context.Context, tx *gorm.DB) (int64, error)

	CountActive(ctx context.Context, tx *gorm.DB) (int, error)

	// ByID reads one key, whatever its status. A soft-deleted key is still
	// addressable: the portal has to be able to show why a key vanished.
	ByID(ctx context.Context, tx *gorm.DB, id int64) (model.Key, error)

	// List is the key browser's bulk fetch: ONE query for the whole page,
	// however many keys it contains.
	List(ctx context.Context, tx *gorm.DB, f KeyFilter) (KeyPage, error)

	// Create inserts a key, returning ErrKeyNameTaken when an active key
	// already holds the name.
	Create(ctx context.Context, tx *gorm.DB, k model.Key) (model.Key, error)

	// Update rewrites a key's metadata and bumps its version.
	//
	// expectedVersion applies optimistic concurrency control when positive, and
	// is skipped when zero — keys.version is CHECKed > 0, so zero cannot be a
	// real version and unambiguously means "no guard". A mismatch returns
	// ErrOptimisticLock.
	Update(ctx context.Context, tx *gorm.DB, k model.Key, expectedVersion int) (model.Key, error)

	// SoftDelete sets status = 'deleted'. The row survives, because history and
	// audit rows reference it and because the name must be reusable — which is
	// exactly what the partial unique index provides.
	SoftDelete(ctx context.Context, tx *gorm.DB, id int64, expectedVersion int) (model.Key, error)

	// RecordHistory appends the key's post-change state to key_history.
	RecordHistory(ctx context.Context, tx *gorm.DB, k model.Key, source model.HistorySource, branchID *int64, actor string) error

	// History reads a key's metadata timeline, newest first.
	History(ctx context.Context, tx *gorm.DB, keyID int64, limit int) ([]KeyHistoryEntry, error)
}

type keyRepository struct{ base }

func ProvideKeyRepository(connector database.GORMConnector) KeyRepository {
	return &keyRepository{base{connector: connector}}
}

// upsertKeySQL is hand-written because GORM v1 has no clause.OnConflict — that
// is a v2 API, and clause.OnConflict appears nowhere in this codebase.
//
// The conflict target is the PARTIAL unique index on active names, so the
// statement needs the same WHERE predicate the index carries.
//
// platforms is accumulated rather than replaced: a key seen in the Flutter file
// and later in the Android file belongs to both, and whichever is imported
// second must not erase the first.
const upsertKeySQL = `
INSERT INTO keys (name, description, platforms, android_name, ios_name,
                  status, sort_index, lokalise_key_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
ON CONFLICT (project_id, name) WHERE status = 'active' DO UPDATE SET
    description  = COALESCE(NULLIF(EXCLUDED.description, ''), keys.description),
    platforms    = ARRAY(SELECT DISTINCT unnest(keys.platforms || EXCLUDED.platforms) ORDER BY 1),
    android_name = COALESCE(EXCLUDED.android_name, keys.android_name),
    ios_name     = COALESCE(EXCLUDED.ios_name, keys.ios_name),
    version      = keys.version + 1,
    updated_at   = now()
-- Same reasoning as translations: do not churn the version, and therefore the
-- conflict anchor, when a re-run changes nothing. platforms is compared after
-- the accumulate so adding a platform still counts as a change.
WHERE keys.description IS DISTINCT FROM COALESCE(NULLIF(EXCLUDED.description, ''), keys.description)
   OR keys.platforms   IS DISTINCT FROM ARRAY(SELECT DISTINCT unnest(keys.platforms || EXCLUDED.platforms) ORDER BY 1)
   OR keys.android_name IS DISTINCT FROM COALESCE(EXCLUDED.android_name, keys.android_name)
   OR keys.ios_name     IS DISTINCT FROM COALESCE(EXCLUDED.ios_name, keys.ios_name)
RETURNING id`

func (r *keyRepository) UpsertByName(ctx context.Context, tx *gorm.DB, k model.Key) (int64, error) {
	platforms := make([]string, len(k.Platforms))
	for i, p := range k.Platforms {
		platforms[i] = string(p)
	}
	if k.Status == "" {
		k.Status = model.KeyStatusActive
	}

	var id int64
	row := r.db(ctx, tx).Raw(upsertKeySQL,
		k.Name, k.Description, pq.Array(platforms),
		k.AndroidName, k.IOSName, string(k.Status), k.SortIndex, k.LokaliseKeyID).Row()

	switch err := row.Scan(&id); {
	case err == nil:
		return id, nil

	case isNoRows(err):
		// ON CONFLICT DO UPDATE ... WHERE <no change> updates no row, so
		// RETURNING yields nothing. That is the desired outcome — the row is
		// already correct and its version was deliberately not churned — but
		// the caller still needs the id, so read it back.
		row = r.db(ctx, tx).Raw(
			`SELECT id FROM keys WHERE name = ? AND status = 'active'`, k.Name).Row()
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("read back unchanged key %q: %w", k.Name, err)
		}
		return id, nil

	default:
		return 0, fmt.Errorf("upsert key %q: %w", k.Name, err)
	}
}

func (r *keyRepository) IDsByName(ctx context.Context, tx *gorm.DB, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	if len(names) == 0 {
		return out, nil
	}

	rows, err := r.db(ctx, tx).Raw(
		`SELECT name, id FROM keys WHERE status = 'active' AND name = ANY($1)`,
		pq.Array(names)).Rows()
	if err != nil {
		return nil, fmt.Errorf("resolve key ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return nil, fmt.Errorf("scan key id: %w", err)
		}
		out[name] = id
	}
	return out, rows.Err()
}

func (r *keyRepository) MaxSortIndex(ctx context.Context, tx *gorm.DB) (int64, error) {
	var highest sql.NullInt64
	row := r.db(ctx, tx).Raw(`SELECT max(sort_index) FROM keys`).Row()
	if err := row.Scan(&highest); err != nil {
		return 0, fmt.Errorf("max sort_index: %w", err)
	}
	return highest.Int64, nil
}

func (r *keyRepository) CountActive(ctx context.Context, tx *gorm.DB) (int, error) {
	var n int
	row := r.db(ctx, tx).Raw(`SELECT count(*) FROM keys WHERE status = 'active'`).Row()
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count active keys: %w", err)
	}
	return n, nil
}

// isNoRows reports whether an error is the driver's empty-result signal.
//
// nil-safe: a caller that reaches this in a switch over a possibly-successful
// scan must get false, not a nil dereference inside err.Error().
func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no rows")
}

// isUniqueViolation reports a SQLSTATE 23505.
//
// The SQLSTATE, not the message. Postgres's wording for a unique violation
// names the index and changes between versions; the code is part of the
// standard and does not.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

const keyColumns = `k.id, k.name, k.description, k.platforms, k.android_name,
    k.ios_name, k.status, k.version, k.sort_index, k.lokalise_key_id,
    k.created_at, k.updated_at`

// scanKey maps one keys row onto model.Key.
//
// android_name and ios_name stay pointers: NULL means "derive from name" and
// a non-NULL value means the derivation was deliberately overridden. Flattening
// them to "" would erase a distinction that cannot be recovered.
func scanKey(row interface{ Scan(...interface{}) error }, extra ...interface{}) (model.Key, error) {
	var (
		k             model.Key
		platforms     []string
		androidName   sql.NullString
		iosName       sql.NullString
		lokaliseKeyID sql.NullInt64
		status        string
	)

	dest := []interface{}{&k.ID, &k.Name, &k.Description, pq.Array(&platforms),
		&androidName, &iosName, &status, &k.Version, &k.SortIndex,
		&lokaliseKeyID, &k.CreatedAt, &k.UpdatedAt}
	dest = append(dest, extra...)

	if err := row.Scan(dest...); err != nil {
		return k, err
	}

	k.Status = model.KeyStatus(status)
	k.Platforms = make([]model.Platform, len(platforms))
	for i, p := range platforms {
		k.Platforms[i] = model.Platform(p)
	}
	if androidName.Valid {
		k.AndroidName = &androidName.String
	}
	if iosName.Valid {
		k.IOSName = &iosName.String
	}
	if lokaliseKeyID.Valid {
		k.LokaliseKeyID = &lokaliseKeyID.Int64
	}
	return k, nil
}

func (r *keyRepository) ByID(ctx context.Context, tx *gorm.DB, id int64) (model.Key, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+keyColumns+` FROM keys k WHERE k.id = ?`, id).Row()

	k, err := scanKey(row)
	switch {
	case err == nil:
		return k, nil
	case isNoRows(err):
		return k, fmt.Errorf("key %d: %w", id, ErrNotFound)
	default:
		return k, fmt.Errorf("read key %d: %w", id, err)
	}
}

// List builds the key browser's query.
//
// ONE round trip for the whole page. The alternative — a query per key to fetch
// its values, or per locale — is thousands of round trips for a browser that
// renders 6,300 rows, which is the difference between a page that loads and a
// page that times out.
//
// The untranslated_in filter LEFT JOINs BOTH translations and
// branch_translations and reuses resolveSQL's exact CASE expression, so
// "untranslated" means the same thing on a branch as it does on master. A
// filter that quietly consulted master while the caller asked for a branch is
// the bug that reaches production.
//
// count(*) OVER () rather than a second COUNT query: two queries can disagree
// under concurrent writes, and a portal showing "1-100 of 6,300" over an empty
// page 63 is showing a lie.
func (r *keyRepository) List(ctx context.Context, tx *gorm.DB, f KeyFilter) (KeyPage, error) {
	var page KeyPage

	statuses := []string{string(model.KeyStatusActive)}
	if f.IncludeDeleted {
		statuses = append(statuses,
			string(model.KeyStatusDraft), string(model.KeyStatusDeleted))
	}

	var (
		joins strings.Builder
		where strings.Builder
		args  []interface{}
	)

	// The status predicate is widened by the BRANCH's opinion, not only
	// master's. A key created on a branch is a draft on master until the merge
	// promotes it, so a plain `status = 'active'` filter would hide, from the
	// branch's own browser, exactly the key the editor just created — while the
	// branch diff showed it. The two views must agree about what the branch
	// contains.
	//
	// bk.status = 'active' rather than "any delta": a branch that soft-deletes a
	// key says so in its delta, and that key is still on master and still
	// matches the status list on its own.
	where.WriteString(` WHERE (k.status = ANY(?::text[])`)
	args = append(args, pq.Array(statuses))
	if f.BranchID > 0 {
		where.WriteString(` OR EXISTS (
            SELECT 1 FROM branch_keys bk
             WHERE bk.branch_id = ? AND bk.key_id = k.id AND bk.status = 'active')`)
		args = append(args, f.BranchID)
	}
	where.WriteString(`)`)

	if f.Platform != "" {
		where.WriteString(` AND ? = ANY(k.platforms)`)
		args = append(args, f.Platform)
	}

	if f.Tag != "" {
		where.WriteString(` AND EXISTS (
            SELECT 1 FROM key_tags kt JOIN tags tg ON tg.id = kt.tag_id
             WHERE kt.key_id = k.id AND tg.name = ?)`)
		args = append(args, f.Tag)
	}

	if f.Search != "" {
		// LIKE metacharacters in the needle are escaped, so searching for a
		// literal "100%" does not match every key in the corpus.
		pattern := "%" + escapeLike(f.Search) + "%"
		where.WriteString(` AND (k.name ILIKE ? ESCAPE '\'
             OR k.description ILIKE ? ESCAPE '\'`)
		args = append(args, pattern, pattern)

		if f.BranchID > 0 {
			// The value search must apply the SAME resolve rule every read
			// does: a branch delta wins where one exists, master otherwise. A
			// master-only search here would make text changed only on the
			// branch unfindable in the branch's own view — and keep finding a
			// key by master text the branch has already rewritten or removed.
			// This is exactly the bug class the doc comment above warns about.
			where.WriteString(`
             OR EXISTS (SELECT 1 FROM branch_translations sbt
                         WHERE sbt.branch_id = ? AND sbt.key_id = k.id
                           AND NOT sbt.is_removed
                           AND sbt.value ILIKE ? ESCAPE '\')
             OR EXISTS (SELECT 1 FROM translations st
                         WHERE st.key_id = k.id AND st.value ILIKE ? ESCAPE '\'
                           AND NOT EXISTS (SELECT 1 FROM branch_translations obt
                                            WHERE obt.branch_id = ?
                                              AND obt.key_id = st.key_id
                                              AND obt.locale_id = st.locale_id)))`)
			args = append(args, f.BranchID, pattern, pattern, f.BranchID)
		} else {
			where.WriteString(`
             OR EXISTS (SELECT 1 FROM translations st
                         WHERE st.key_id = k.id AND st.value ILIKE ? ESCAPE '\'))`)
			args = append(args, pattern)
		}
	}

	if f.UntranslatedIn > 0 {
		joins.WriteString(`
  LEFT JOIN translations ut
    ON ut.key_id = k.id AND ut.locale_id = ?
  LEFT JOIN branch_translations ubt
    ON ubt.key_id = k.id AND ubt.locale_id = ? AND ubt.branch_id = ?`)
		args = append(args, f.UntranslatedIn, f.UntranslatedIn, f.BranchID)

		// Exactly resolveSQL's "found" expression, negated. A tombstone on the
		// branch counts as untranslated; a master row holding '' does not.
		where.WriteString(` AND NOT (CASE
            WHEN ubt.key_id IS NOT NULL THEN NOT ubt.is_removed
            ELSE ut.key_id IS NOT NULL
        END)`)
	}

	// The join arguments are bound before the where arguments in the string, so
	// they must precede them in args too. Rebuild in statement order.
	var ordered []interface{}
	if f.UntranslatedIn > 0 {
		ordered = append(ordered, args[len(args)-3:]...)
		args = args[:len(args)-3]
	}
	ordered = append(ordered, args...)

	query := `SELECT ` + keyColumns + `, count(*) OVER () AS total
  FROM keys k` + joins.String() + where.String() + `
 ORDER BY k.sort_index, k.id`

	if f.Limit > 0 {
		query += ` LIMIT ?`
		ordered = append(ordered, f.Limit)
	}
	if f.Offset > 0 {
		query += ` OFFSET ?`
		ordered = append(ordered, f.Offset)
	}

	rows, err := r.db(ctx, tx).Raw(query, ordered...).Rows()
	if err != nil {
		return page, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()

	page.Keys = make([]model.Key, 0, 512)
	for rows.Next() {
		var total int
		k, err := scanKey(rows, &total)
		if err != nil {
			return page, fmt.Errorf("scan key: %w", err)
		}
		page.Total = total
		page.Keys = append(page.Keys, k)
	}
	return page, rows.Err()
}

// escapeLike neutralises the three characters ILIKE treats specially.
func escapeLike(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}

// createKeySQL inserts a key, or yields nothing when the name is taken.
//
// The conflict target is the PARTIAL unique index on active names, so the
// statement carries the same predicate the index does. DO NOTHING then RETURNING
// makes "the name is taken" a missing row rather than a driver error to
// pattern-match — the same move as createTagSQL.
const createKeySQL = `
INSERT INTO keys (name, description, platforms, android_name, ios_name,
                  status, sort_index, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6,
        COALESCE((SELECT max(sort_index) FROM keys), 0) + 100,
        now(), now())
ON CONFLICT (project_id, name) WHERE status = 'active' DO NOTHING
RETURNING id, name, description, platforms, android_name, ios_name, status,
          version, sort_index, lokalise_key_id, created_at, updated_at`

func (r *keyRepository) Create(ctx context.Context, tx *gorm.DB, k model.Key) (model.Key, error) {
	platforms := make([]string, len(k.Platforms))
	for i, p := range k.Platforms {
		platforms[i] = string(p)
	}
	if k.Status == "" {
		k.Status = model.KeyStatusActive
	}

	// sort_index is allocated as max+100 inside the statement, in gaps, so
	// inserting a key between two others later stays one UPDATE rather than a
	// renumber of 6,300 rows.
	row := r.db(ctx, tx).Raw(createKeySQL, k.Name, k.Description, pq.Array(platforms),
		k.AndroidName, k.IOSName, string(k.Status)).Row()

	created, err := scanKey(row)
	switch {
	case err == nil:
		return created, nil
	case isNoRows(err):
		return created, fmt.Errorf("create key %q: %w", k.Name, ErrKeyNameTaken)
	default:
		return created, fmt.Errorf("create key %q: %w", k.Name, err)
	}
}

const updateKeySQL = `
UPDATE keys k
   SET name = $2, description = $3, platforms = $4, android_name = $5,
       ios_name = $6, status = $7, version = k.version + 1, updated_at = now()
 WHERE k.id = $1
   AND ($8 = 0 OR k.version = $8)
RETURNING k.id, k.name, k.description, k.platforms, k.android_name, k.ios_name,
          k.status, k.version, k.sort_index, k.lokalise_key_id,
          k.created_at, k.updated_at`

func (r *keyRepository) Update(
	ctx context.Context, tx *gorm.DB, k model.Key, expectedVersion int,
) (model.Key, error) {
	platforms := make([]string, len(k.Platforms))
	for i, p := range k.Platforms {
		platforms[i] = string(p)
	}

	row := r.db(ctx, tx).Raw(updateKeySQL, k.ID, k.Name, k.Description,
		pq.Array(platforms), k.AndroidName, k.IOSName, string(k.Status),
		expectedVersion).Row()

	updated, err := scanKey(row)
	switch {
	case err == nil:
		return updated, nil

	case isUniqueViolation(err):
		// A rename onto a name an active key already holds. The partial unique
		// index is the authority here; a pre-check would be a decoration,
		// because the other key can appear between the check and the write.
		return updated, fmt.Errorf("rename key %d to %q: %w", k.ID, k.Name, ErrKeyNameTaken)

	case isNoRows(err):
		// No row came back for one of two reasons, and they are different
		// facts: the key is gone, or someone else moved it first.
		return updated, r.classifyMiss(ctx, tx, k.ID, expectedVersion)

	default:
		return updated, fmt.Errorf("update key %d: %w", k.ID, err)
	}
}

const softDeleteKeySQL = `
UPDATE keys k
   SET status = 'deleted', version = k.version + 1, updated_at = now()
 WHERE k.id = $1
   AND k.status <> 'deleted'
   AND ($2 = 0 OR k.version = $2)
RETURNING k.id, k.name, k.description, k.platforms, k.android_name, k.ios_name,
          k.status, k.version, k.sort_index, k.lokalise_key_id,
          k.created_at, k.updated_at`

func (r *keyRepository) SoftDelete(
	ctx context.Context, tx *gorm.DB, id int64, expectedVersion int,
) (model.Key, error) {
	row := r.db(ctx, tx).Raw(softDeleteKeySQL, id, expectedVersion).Row()

	deleted, err := scanKey(row)
	switch {
	case err == nil:
		return deleted, nil
	case isNoRows(err):
		return deleted, r.classifyMiss(ctx, tx, id, expectedVersion)
	default:
		return deleted, fmt.Errorf("delete key %d: %w", id, err)
	}
}

// classifyMiss decides whether a version-guarded write missed because the row
// is gone or because another writer moved it.
//
// Returning ErrNotFound for both would be wrong in the way that matters: a 404
// tells an editor their key vanished, when in fact a colleague saved first and
// the correct answer is a 409 that shows them both versions.
func (r *keyRepository) classifyMiss(ctx context.Context, tx *gorm.DB, id int64, expectedVersion int) error {
	var (
		version int
		status  string
	)
	row := r.db(ctx, tx).Raw(`SELECT version, status FROM keys WHERE id = ?`, id).Row()
	switch err := row.Scan(&version, &status); {
	case err == nil:
	case isNoRows(err):
		return fmt.Errorf("key %d: %w", id, ErrNotFound)
	default:
		return fmt.Errorf("read key %d: %w", id, err)
	}

	if expectedVersion > 0 && version != expectedVersion {
		return fmt.Errorf("key %d is at version %d, not %d: %w",
			id, version, expectedVersion, ErrOptimisticLock)
	}
	// The row exists at the expected version, so the WHERE clause failed on the
	// status predicate — the key is already deleted.
	return fmt.Errorf("key %d is already deleted: %w", id, ErrNotFound)
}

func (r *keyRepository) RecordHistory(
	ctx context.Context, tx *gorm.DB, k model.Key,
	source model.HistorySource, branchID *int64, actor string,
) error {
	platforms := make([]string, len(k.Platforms))
	for i, p := range k.Platforms {
		platforms[i] = string(p)
	}

	err := r.db(ctx, tx).Exec(`
		INSERT INTO key_history (key_id, name, description, platforms,
		                         android_name, ios_name, status, version,
		                         source, branch_id, changed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		k.ID, k.Name, k.Description, pq.Array(platforms), k.AndroidName,
		k.IOSName, string(k.Status), k.Version, string(source), branchID, actor).Error
	if err != nil {
		return fmt.Errorf("record key history for %d: %w", k.ID, err)
	}
	return nil
}

func (r *keyRepository) History(
	ctx context.Context, tx *gorm.DB, keyID int64, limit int,
) ([]KeyHistoryEntry, error) {
	if limit <= 0 {
		limit = 200
	}

	rows, err := r.db(ctx, tx).Raw(`
		SELECT id, key_id, name, description, platforms, status, version,
		       source, branch_id, changed_by, changed_at
		  FROM key_history
		 WHERE key_id = ?
		 ORDER BY changed_at DESC, id DESC
		 LIMIT ?`, keyID, limit).Rows()
	if err != nil {
		return nil, fmt.Errorf("read key history for %d: %w", keyID, err)
	}
	defer rows.Close()

	var out []KeyHistoryEntry
	for rows.Next() {
		var (
			e         KeyHistoryEntry
			platforms []string
		)
		if err := rows.Scan(&e.ID, &e.KeyID, &e.Name, &e.Description,
			pq.Array(&platforms), &e.Status, &e.Version, &e.Source,
			&e.BranchID, &e.ChangedBy, &e.ChangedAt); err != nil {
			return nil, fmt.Errorf("scan key history: %w", err)
		}
		e.Platforms = make([]model.Platform, len(platforms))
		for i, p := range platforms {
			e.Platforms[i] = model.Platform(p)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
