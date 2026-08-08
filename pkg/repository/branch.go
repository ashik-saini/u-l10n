package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// Branch is a named workspace holding copy-on-write deltas over master.
type Branch struct {
	ID           int64
	Name         string
	Description  string
	Status       string
	CreatedBy    string
	CreatedAt    time.Time
	LastEditedAt *time.Time
	MergedAt     *time.Time
}

const (
	BranchStatusOpen   = "open"
	BranchStatusMerged = "merged"
	BranchStatusClosed = "closed"
)

// ResolvedValue is one (key, locale) as seen from a branch.
type ResolvedValue struct {
	Value string
	// Found is false for untranslated — which on a branch means either no
	// delta and no master row, or a delta tombstone. Absent and empty remain
	// different facts here exactly as they are on master.
	Found bool
	// FromDelta reports whether the branch overrides master for this pair.
	// The portal uses it to mark changed cells.
	FromDelta bool
	// MasterVersion is master's current version, or 0 when no master row
	// exists. This is what a writer captures as base_master_version.
	MasterVersion int
}

// BranchRepository owns branches and their copy-on-write deltas.
type BranchRepository interface {
	Create(ctx context.Context, tx *gorm.DB, name, description, createdBy string) (Branch, error)
	ByName(ctx context.Context, tx *gorm.DB, name string) (Branch, error)
	List(ctx context.Context, tx *gorm.DB, status string) ([]Branch, error)

	// Resolve reads one (key, locale) through the branch.
	Resolve(ctx context.Context, tx *gorm.DB, branchID, keyID int64, localeID int16) (ResolvedValue, error)

	// SetValue writes a delta. See the implementation note on why
	// base_master_version is captured only on the FIRST touch.
	SetValue(ctx context.Context, tx *gorm.DB, branchID, keyID int64, localeID int16, value string, hint model.RenderHint, actor string) error

	// RemoveValue writes a tombstone: this branch deletes the translation that
	// exists on master. Distinct from writing an empty string.
	RemoveValue(ctx context.Context, tx *gorm.DB, branchID, keyID int64, localeID int16, actor string) error

	// ChangedCount reports how many value deltas a branch carries.
	ChangedCount(ctx context.Context, tx *gorm.DB, branchID int64) (int, error)
}

type branchRepository struct{ base }

func ProvideBranchRepository(connector database.GORMConnector) BranchRepository {
	return &branchRepository{base{connector: connector}}
}

const branchColumns = `id, name, description, status, created_by, created_at, last_edited_at, merged_at`

func scanBranch(row interface{ Scan(...interface{}) error }) (Branch, error) {
	var b Branch
	err := row.Scan(&b.ID, &b.Name, &b.Description, &b.Status,
		&b.CreatedBy, &b.CreatedAt, &b.LastEditedAt, &b.MergedAt)
	return b, err
}

func (r *branchRepository) Create(ctx context.Context, tx *gorm.DB, name, description, createdBy string) (Branch, error) {
	row := r.db(ctx, tx).Raw(`
		INSERT INTO branches (name, description, created_by)
		VALUES ($1, $2, $3)
		RETURNING `+branchColumns, name, description, createdBy).Row()

	b, err := scanBranch(row)
	if err != nil {
		return b, fmt.Errorf("create branch %q: %w", name, err)
	}
	return b, nil
}

func (r *branchRepository) ByName(ctx context.Context, tx *gorm.DB, name string) (Branch, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+branchColumns+` FROM branches WHERE name = $1`, name).Row()

	b, err := scanBranch(row)
	switch {
	case err == nil:
		return b, nil
	case isNoRows(err):
		return b, fmt.Errorf("branch %q: %w", name, ErrNotFound)
	default:
		return b, fmt.Errorf("branch %q: %w", name, err)
	}
}

func (r *branchRepository) List(ctx context.Context, tx *gorm.DB, status string) ([]Branch, error) {
	q := `SELECT ` + branchColumns + ` FROM branches`
	args := []interface{}{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`

	rows, err := r.db(ctx, tx).Raw(q, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	var out []Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan branch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// resolveSQL implements the copy-on-write read.
//
//	delta row present?  -> use it (is_removed means untranslated)
//	otherwise           -> use the master row
//
// A branch does NOT copy master's ~36,000 values; it stores only what changed.
// Master reads take this identical path with branch_id matching nothing, so
// there is ONE read implementation rather than two that drift apart.
//
// master.version is returned alongside because a writer must capture it as
// base_master_version, and COALESCE(...,0) makes "no master row" an ordinary
// value rather than a NULL special case — which is what later reduces the whole
// conflict rule to a single integer comparison.
const resolveSQL = `
SELECT
    COALESCE(bt.value, t.value, '')                    AS value,
    CASE
        WHEN bt.key_id IS NOT NULL THEN NOT bt.is_removed
        ELSE t.key_id IS NOT NULL
    END                                                AS found,
    (bt.key_id IS NOT NULL)                            AS from_delta,
    COALESCE(t.version, 0)                             AS master_version
  FROM (SELECT $2::bigint AS key_id, $3::smallint AS locale_id) AS want
  LEFT JOIN translations t
    ON t.key_id = want.key_id AND t.locale_id = want.locale_id
  LEFT JOIN branch_translations bt
    ON bt.key_id = want.key_id AND bt.locale_id = want.locale_id
   AND bt.branch_id = $1`

func (r *branchRepository) Resolve(
	ctx context.Context, tx *gorm.DB, branchID, keyID int64, localeID int16,
) (ResolvedValue, error) {
	var v ResolvedValue
	row := r.db(ctx, tx).Raw(resolveSQL, branchID, keyID, localeID).Row()
	if err := row.Scan(&v.Value, &v.Found, &v.FromDelta, &v.MasterVersion); err != nil {
		return v, fmt.Errorf("resolve (%d,%d) on branch %d: %w", keyID, localeID, branchID, err)
	}
	// A tombstone resolves to untranslated, and an untranslated pair has no
	// value to report.
	if !v.Found {
		v.Value = ""
	}
	return v, nil
}

// setValueSQL writes a delta, capturing base_master_version ONLY on insert.
//
// That "only on insert" is the crux of the whole merge design. base_master_version
// records what master looked like when this branch FIRST touched the pair — the
// starting point, not the work. Refreshing it on later edits would quietly adopt
// master's newer version as the base and make a genuine conflict look clean.
//
// The subquery reads master inside the same statement so the captured value
// cannot skew between a separate read and this write.
const setValueSQL = `
INSERT INTO branch_translations
    (branch_id, key_id, locale_id, value, render_hint, is_removed, base_master_version, updated_by)
VALUES
    ($1, $2, $3, $4, $5, FALSE,
     COALESCE((SELECT version FROM translations WHERE key_id = $2 AND locale_id = $3), 0),
     $6)
ON CONFLICT (branch_id, key_id, locale_id) DO UPDATE SET
    value       = EXCLUDED.value,
    render_hint = EXCLUDED.render_hint,
    is_removed  = FALSE,
    -- deliberately NOT updated: base_master_version stays as first captured
    updated_by  = EXCLUDED.updated_by,
    updated_at  = now()`

func (r *branchRepository) SetValue(
	ctx context.Context, tx *gorm.DB, branchID, keyID int64, localeID int16,
	value string, hint model.RenderHint, actor string,
) error {
	if hint == "" {
		hint = model.RenderHintPlain
	}

	db := r.db(ctx, tx)
	if err := db.Exec(setValueSQL, branchID, keyID, localeID, value, string(hint), actor).Error; err != nil {
		return fmt.Errorf("set branch value (%d,%d): %w", keyID, localeID, err)
	}
	return r.touch(ctx, db, branchID)
}

// removeValueSQL writes a tombstone. value must be NULL to satisfy
// branch_translations_removed_value_check, which enforces that a removal
// carries no value and an edit always does.
const removeValueSQL = `
INSERT INTO branch_translations
    (branch_id, key_id, locale_id, value, is_removed, base_master_version, updated_by)
VALUES
    ($1, $2, $3, NULL, TRUE,
     COALESCE((SELECT version FROM translations WHERE key_id = $2 AND locale_id = $3), 0),
     $4)
ON CONFLICT (branch_id, key_id, locale_id) DO UPDATE SET
    value      = NULL,
    is_removed = TRUE,
    updated_by = EXCLUDED.updated_by,
    updated_at = now()`

func (r *branchRepository) RemoveValue(
	ctx context.Context, tx *gorm.DB, branchID, keyID int64, localeID int16, actor string,
) error {
	db := r.db(ctx, tx)
	if err := db.Exec(removeValueSQL, branchID, keyID, localeID, actor).Error; err != nil {
		return fmt.Errorf("remove branch value (%d,%d): %w", keyID, localeID, err)
	}
	return r.touch(ctx, db, branchID)
}

// touch records that the branch changed.
//
// This is what invalidates an approval: the merge transaction compares
// last_edited_at against merge_requests.approved_at, so a diff cannot be
// approved and then quietly altered. Every delta write must call it.
func (r *branchRepository) touch(ctx context.Context, db *gorm.DB, branchID int64) error {
	if err := db.Exec(
		`UPDATE branches SET last_edited_at = now() WHERE id = ?`, branchID).Error; err != nil {
		return fmt.Errorf("touch branch %d: %w", branchID, err)
	}
	return nil
}

func (r *branchRepository) ChangedCount(ctx context.Context, tx *gorm.DB, branchID int64) (int, error) {
	var n int
	row := r.db(ctx, tx).Raw(
		`SELECT count(*) FROM branch_translations WHERE branch_id = ?`, branchID).Row()
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count branch deltas: %w", err)
	}
	return n, nil
}
