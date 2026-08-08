package repository

import (
	"context"
	"fmt"

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

func (r *translationRepository) Count(ctx context.Context, tx *gorm.DB) (int, error) {
	var n int
	row := r.db(ctx, tx).Raw(`SELECT count(*) FROM translations`).Row()
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count translations: %w", err)
	}
	return n, nil
}
