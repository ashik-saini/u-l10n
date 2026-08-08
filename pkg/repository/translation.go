package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/lib/pq"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// TranslationRepository owns the translations table.
type TranslationRepository interface {
	// Get returns the value and whether a row exists.
	//
	// The (value, found, err) signature is deliberate. A key that is absent and
	// a key whose value is "" are different facts — absent means untranslated
	// and is omitted from exports, "" means deliberately blank and exports as
	// "". Go's zero value for string is "", so returning a bare string would
	// silently merge the two and corrupt roughly 3,600 values in ms-MY alone.
	Get(ctx context.Context, tx *gorm.DB, keyID int64, localeID int16) (value string, found bool, err error)

	// Upsert writes a value unconditionally. Used by the importer, which is the
	// authority for its own run; interactive edits must use UpdateWithVersion.
	Upsert(ctx context.Context, tx *gorm.DB, t model.Translation) error

	// UpsertBatch writes many values in one statement.
	UpsertBatch(ctx context.Context, tx *gorm.DB, ts []model.Translation) error

	// UpdateWithVersion applies optimistic concurrency control: it succeeds only
	// if the row is still at expectedVersion, and returns ErrOptimisticLock
	// otherwise. Zero affected rows IS the conflict signal.
	UpdateWithVersion(ctx context.Context, tx *gorm.DB, t model.Translation, expectedVersion int) error

	Count(ctx context.Context, tx *gorm.DB) (int, error)

	// CreateCell inserts the FIRST value for a pair, and refuses to overwrite.
	//
	// It is the create half of optimistic concurrency control: a client sending
	// base_version 0 is asserting "there is no row here", and if another editor
	// created one first that assertion is false. Upsert would clobber their work
	// silently; this returns ErrOptimisticLock so the same 409 that guards an
	// edit also guards a create.
	CreateCell(ctx context.Context, tx *gorm.DB, t model.Translation) error

	// GetCell reads one (key, locale) with everything an editor needs to act on
	// a conflict: the value, whether a row exists at all, and the version that
	// is the optimistic-concurrency anchor.
	//
	// Distinct from Get, which predates the portal and answers only the
	// importer's question. Get is left alone because the seed path is proven
	// against it.
	GetCell(ctx context.Context, tx *gorm.DB, keyID int64, localeID int16) (Cell, error)

	// ResolveMany reads MANY (key, locale) pairs in ONE query, through the
	// copy-on-write branch view.
	//
	// This is the key browser's value fetch: ~6,300 keys x 6 locales arrive as
	// a single round trip. A query per key, or per locale, would be thousands of
	// round trips for one page. Pairs that resolve to untranslated are present
	// in the result with Found false, never absent — the caller must be able to
	// tell "no value" from "not asked for".
	ResolveMany(ctx context.Context, tx *gorm.DB, branchID int64, keyIDs []int64, localeIDs []int16) (map[Cell2Key]Cell, error)

	// Delete removes the row entirely, making the pair UNTRANSLATED.
	//
	// Emphatically not the same as writing "": an absent row is omitted from
	// the export, an empty one is exported as "". Returns ErrNotFound when there
	// was nothing to remove, so a caller can tell the two outcomes apart.
	Delete(ctx context.Context, tx *gorm.DB, keyID int64, localeID int16) error

	// RecordHistory appends to the insert-only translation_history table.
	//
	// value is a pointer because NULL distinguishes "became untranslated" from
	// "became the empty string" — the same three-state rule, carried into the
	// audit trail where it matters most.
	RecordHistory(ctx context.Context, tx *gorm.DB, keyID int64, localeID int16, value *string, hint model.RenderHint, version int, source model.HistorySource, branchID *int64, actor string) error

	// History reads a key's value timeline, newest first. localeID zero means
	// every locale.
	History(ctx context.Context, tx *gorm.DB, keyID int64, localeID int16, limit int) ([]TranslationHistoryEntry, error)
}

// Cell2Key identifies one (key, locale) pair in a bulk result.
type Cell2Key struct {
	KeyID    int64
	LocaleID int16
}

// Cell is one (key, locale) as the portal must see it.
//
// Value is only meaningful when Found is true. The two fields exist separately
// because a bare string cannot express the three states this schema turns on:
// no row (untranslated, omitted from the export), a row holding "" (deliberately
// blank, exported as ""), and a row holding text.
type Cell struct {
	Value string
	Found bool

	RenderHint model.RenderHint

	// Version is MASTER's version, and zero when master has no row. It is the
	// optimistic-concurrency anchor a client sends back as base_version.
	Version int

	// FromBranch reports that a branch delta overrides master for this pair.
	// The portal marks those cells as changed.
	FromBranch bool

	UpdatedBy string
	UpdatedAt *time.Time
}

// TranslationHistoryEntry is one row of the append-only translation_history.
type TranslationHistoryEntry struct {
	ID       int64
	KeyID    int64
	LocaleID int16
	// LocaleCode is joined in, because a timeline showing locale id 4 is a
	// timeline nobody can read.
	LocaleCode string
	// Value is nil for "became untranslated". See RecordHistory.
	Value      *string
	RenderHint string
	Version    int
	Source     string
	BranchID   *int64
	ChangedBy  string
	ChangedAt  time.Time
}

type translationRepository struct{ base }

func ProvideTranslationRepository(connector database.GORMConnector) TranslationRepository {
	return &translationRepository{base{connector: connector}}
}

func (r *translationRepository) Get(ctx context.Context, tx *gorm.DB, keyID int64, localeID int16) (string, bool, error) {
	var value string
	row := r.db(ctx, tx).Raw(
		`SELECT value FROM translations WHERE key_id = ? AND locale_id = ?`,
		keyID, localeID).Row()

	switch err := row.Scan(&value); {
	case err == nil:
		return value, true, nil
	case isNoRows(err):
		// Not an error: absence is a meaningful state.
		return "", false, nil
	default:
		return "", false, fmt.Errorf("get translation (%d,%d): %w", keyID, localeID, err)
	}
}

const upsertTranslationSQL = `
INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
VALUES ($1, $2, $3, $4, 1, $5, now())
ON CONFLICT (key_id, locale_id) DO UPDATE SET
    value       = EXCLUDED.value,
    render_hint = EXCLUDED.render_hint,
    version     = translations.version + 1,
    updated_by  = EXCLUDED.updated_by,
    updated_at  = now()
-- Only fire when something actually CHANGED. Without this guard a re-run of the
-- idempotent importer bumps every version, and since version is the optimistic
-- concurrency anchor that hands a spurious 409 to every editor with a value
-- open. IS DISTINCT FROM rather than <> so NULLs compare correctly.
WHERE translations.value       IS DISTINCT FROM EXCLUDED.value
   OR translations.render_hint IS DISTINCT FROM EXCLUDED.render_hint`

func (r *translationRepository) Upsert(ctx context.Context, tx *gorm.DB, t model.Translation) error {
	if t.RenderHint == "" {
		t.RenderHint = model.RenderHintPlain
	}
	err := r.db(ctx, tx).Exec(upsertTranslationSQL,
		t.KeyID, t.LocaleID, t.Value, string(t.RenderHint), t.UpdatedBy).Error
	if err != nil {
		return fmt.Errorf("upsert translation (%d,%d): %w", t.KeyID, t.LocaleID, err)
	}
	return nil
}

// UpsertBatch writes through UNNEST, the house bulk-insert idiom (see
// u-reward/pkg/repository/user_participation.go). One statement instead of
// 36,000 keeps a full import inside a single fast transaction.
const upsertTranslationBatchSQL = `
INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
SELECT k, l, v, h, 1, $5, now()
FROM unnest($1::bigint[], $2::smallint[], $3::text[], $4::text[]) AS t(k, l, v, h)
ON CONFLICT (key_id, locale_id) DO UPDATE SET
    value       = EXCLUDED.value,
    render_hint = EXCLUDED.render_hint,
    version     = translations.version + 1,
    updated_by  = EXCLUDED.updated_by,
    updated_at  = now()
-- Only fire when something actually CHANGED. Without this guard a re-run of the
-- idempotent importer bumps every version, and since version is the optimistic
-- concurrency anchor that hands a spurious 409 to every editor with a value
-- open. IS DISTINCT FROM rather than <> so NULLs compare correctly.
WHERE translations.value       IS DISTINCT FROM EXCLUDED.value
   OR translations.render_hint IS DISTINCT FROM EXCLUDED.render_hint`

func (r *translationRepository) UpsertBatch(ctx context.Context, tx *gorm.DB, ts []model.Translation) error {
	if len(ts) == 0 {
		return nil
	}

	keyIDs := make([]int64, len(ts))
	localeIDs := make([]int64, len(ts))
	values := make([]string, len(ts))
	hints := make([]string, len(ts))
	updatedBy := ts[0].UpdatedBy

	for i, t := range ts {
		hint := t.RenderHint
		if hint == "" {
			hint = model.RenderHintPlain
		}
		keyIDs[i] = t.KeyID
		localeIDs[i] = int64(t.LocaleID)
		values[i] = t.Value
		hints[i] = string(hint)
	}

	err := r.db(ctx, tx).Exec(upsertTranslationBatchSQL,
		pq.Array(keyIDs), pq.Array(localeIDs),
		pq.Array(values), pq.Array(hints), updatedBy).Error
	if err != nil {
		return fmt.Errorf("upsert %d translations: %w", len(ts), err)
	}
	return nil
}

const updateWithVersionSQL = `
UPDATE translations
   SET value = $1, render_hint = $2, version = version + 1,
       updated_by = $3, updated_at = now()
 WHERE key_id = $4 AND locale_id = $5 AND version = $6`

func (r *translationRepository) UpdateWithVersion(
	ctx context.Context, tx *gorm.DB, t model.Translation, expectedVersion int,
) error {
	if t.RenderHint == "" {
		t.RenderHint = model.RenderHintPlain
	}

	res := r.db(ctx, tx).Exec(updateWithVersionSQL,
		t.Value, string(t.RenderHint), t.UpdatedBy, t.KeyID, t.LocaleID, expectedVersion)
	if res.Error != nil {
		return fmt.Errorf("update translation (%d,%d): %w", t.KeyID, t.LocaleID, res.Error)
	}

	// Zero rows means the version moved: another writer got there first. This
	// is the whole point of OCC — do not retry, surface it, let a human choose.
	if res.RowsAffected == 0 {
		return ErrOptimisticLock
	}
	return nil
}

const createCellSQL = `
INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
VALUES ($1, $2, $3, $4, 1, $5, now())
ON CONFLICT (key_id, locale_id) DO NOTHING`

func (r *translationRepository) CreateCell(ctx context.Context, tx *gorm.DB, t model.Translation) error {
	if t.RenderHint == "" {
		t.RenderHint = model.RenderHintPlain
	}

	res := r.db(ctx, tx).Exec(createCellSQL,
		t.KeyID, t.LocaleID, t.Value, string(t.RenderHint), t.UpdatedBy)
	if res.Error != nil {
		return fmt.Errorf("create translation (%d,%d): %w", t.KeyID, t.LocaleID, res.Error)
	}

	// DO NOTHING affected no row, so a row was already there. Same signal as
	// UpdateWithVersion's zero rows, and the same conclusion: another writer got
	// here first, do not retry, surface it.
	if res.RowsAffected == 0 {
		return ErrOptimisticLock
	}
	return nil
}

const getCellSQL = `
SELECT value, render_hint, version, updated_by, updated_at
  FROM translations
 WHERE key_id = $1 AND locale_id = $2`

func (r *translationRepository) GetCell(
	ctx context.Context, tx *gorm.DB, keyID int64, localeID int16,
) (Cell, error) {
	var (
		c         Cell
		hint      string
		updatedAt time.Time
	)

	row := r.db(ctx, tx).Raw(getCellSQL, keyID, localeID).Row()
	switch err := row.Scan(&c.Value, &hint, &c.Version, &c.UpdatedBy, &updatedAt); {
	case err == nil:
		c.Found = true
		c.RenderHint = model.RenderHint(hint)
		c.UpdatedAt = &updatedAt
		return c, nil
	case isNoRows(err):
		// Absence is a state, not an error. Version stays 0, which is what a
		// writer captures as the base for a pair that does not exist yet.
		return Cell{}, nil
	default:
		return c, fmt.Errorf("read cell (%d,%d): %w", keyID, localeID, err)
	}
}

// resolveManySQL is resolveSQL widened from one pair to a whole page.
//
// The generated `want` set is the cross product of the requested keys and
// locales, so EVERY asked-for pair comes back — including the untranslated ones,
// which are exactly the ones the browser has to render as empty cells. Dropping
// them would make "not asked for" and "no value" indistinguishable at the
// caller.
//
// The two LEFT JOINs and the CASE are character-for-character the same rule as
// resolveSQL. A second, subtly different resolution rule is how a branch read
// starts quietly returning master's values.
const resolveManySQL = `
SELECT want.key_id, want.locale_id,
    COALESCE(bt.value, t.value, '')                     AS value,
    CASE
        WHEN bt.key_id IS NOT NULL THEN NOT bt.is_removed
        ELSE t.key_id IS NOT NULL
    END                                                 AS found,
    (bt.key_id IS NOT NULL)                             AS from_branch,
    COALESCE(bt.render_hint, t.render_hint, 'plain')    AS render_hint,
    COALESCE(t.version, 0)                              AS master_version,
    COALESCE(bt.updated_by, t.updated_by, '')           AS updated_by,
    COALESCE(bt.updated_at, t.updated_at)               AS updated_at
  FROM (
        SELECT k AS key_id, l AS locale_id
          FROM unnest($2::bigint[]) AS k
         CROSS JOIN unnest($3::smallint[]) AS l
  ) AS want
  LEFT JOIN translations t
    ON t.key_id = want.key_id AND t.locale_id = want.locale_id
  LEFT JOIN branch_translations bt
    ON bt.key_id = want.key_id AND bt.locale_id = want.locale_id
   AND bt.branch_id = $1`

func (r *translationRepository) ResolveMany(
	ctx context.Context, tx *gorm.DB, branchID int64, keyIDs []int64, localeIDs []int16,
) (map[Cell2Key]Cell, error) {
	out := make(map[Cell2Key]Cell, len(keyIDs)*len(localeIDs))
	if len(keyIDs) == 0 || len(localeIDs) == 0 {
		// An empty page is a normal outcome of a filter that matched nothing,
		// and it should cost no query at all.
		return out, nil
	}

	// lib/pq has a fast path for []int64; the ::smallint[] cast narrows on the
	// server. Same handling as UpsertBatch's locale ids.
	locales := make([]int64, len(localeIDs))
	for i, id := range localeIDs {
		locales[i] = int64(id)
	}

	rows, err := r.db(ctx, tx).Raw(resolveManySQL,
		branchID, pq.Array(keyIDs), pq.Array(locales)).Rows()
	if err != nil {
		return nil, fmt.Errorf("resolve %d keys x %d locales on branch %d: %w",
			len(keyIDs), len(localeIDs), branchID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			k         Cell2Key
			c         Cell
			hint      string
			updatedAt pq.NullTime
		)
		if err := rows.Scan(&k.KeyID, &k.LocaleID, &c.Value, &c.Found,
			&c.FromBranch, &hint, &c.Version, &c.UpdatedBy, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan resolved cell: %w", err)
		}
		c.RenderHint = model.RenderHint(hint)
		if updatedAt.Valid {
			t := updatedAt.Time
			c.UpdatedAt = &t
		}
		if !c.Found {
			// A tombstone or a missing row has no value to report, and leaving
			// COALESCE's '' in place would read as a deliberate blank.
			c.Value = ""
		}
		out[k] = c
	}
	return out, rows.Err()
}

func (r *translationRepository) Delete(
	ctx context.Context, tx *gorm.DB, keyID int64, localeID int16,
) error {
	res := r.db(ctx, tx).Exec(
		`DELETE FROM translations WHERE key_id = ? AND locale_id = ?`, keyID, localeID)
	if res.Error != nil {
		return fmt.Errorf("delete translation (%d,%d): %w", keyID, localeID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("translation (%d,%d): %w", keyID, localeID, ErrNotFound)
	}
	return nil
}

func (r *translationRepository) RecordHistory(
	ctx context.Context, tx *gorm.DB, keyID int64, localeID int16, value *string,
	hint model.RenderHint, version int, source model.HistorySource,
	branchID *int64, actor string,
) error {
	if hint == "" {
		hint = model.RenderHintPlain
	}

	err := r.db(ctx, tx).Exec(`
		INSERT INTO translation_history
		    (key_id, locale_id, value, render_hint, version, source, branch_id, changed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		keyID, localeID, value, string(hint), version, string(source), branchID, actor).Error
	if err != nil {
		return fmt.Errorf("record translation history (%d,%d): %w", keyID, localeID, err)
	}
	return nil
}

func (r *translationRepository) History(
	ctx context.Context, tx *gorm.DB, keyID int64, localeID int16, limit int,
) ([]TranslationHistoryEntry, error) {
	if limit <= 0 {
		limit = 200
	}

	rows, err := r.db(ctx, tx).Raw(`
		SELECT h.id, h.key_id, h.locale_id, l.code, h.value, h.render_hint,
		       h.version, h.source, h.branch_id, h.changed_by, h.changed_at
		  FROM translation_history h
		  JOIN locales l ON l.id = h.locale_id
		 WHERE h.key_id = ?
		   AND (? = 0 OR h.locale_id = ?)
		 ORDER BY h.changed_at DESC, h.id DESC
		 LIMIT ?`, keyID, localeID, localeID, limit).Rows()
	if err != nil {
		return nil, fmt.Errorf("read translation history for %d: %w", keyID, err)
	}
	defer rows.Close()

	var out []TranslationHistoryEntry
	for rows.Next() {
		var (
			e     TranslationHistoryEntry
			value sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.KeyID, &e.LocaleID, &e.LocaleCode, &value,
			&e.RenderHint, &e.Version, &e.Source, &e.BranchID,
			&e.ChangedBy, &e.ChangedAt); err != nil {
			return nil, fmt.Errorf("scan translation history: %w", err)
		}
		if value.Valid {
			// Only a non-NULL value becomes a pointer. NULL stays nil, which is
			// what tells a reader the pair became untranslated rather than blank.
			v := value.String
			e.Value = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *translationRepository) Count(ctx context.Context, tx *gorm.DB) (int, error) {
	var n int
	row := r.db(ctx, tx).Raw(`SELECT count(*) FROM translations`).Row()
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count translations: %w", err)
	}
	return n, nil
}
