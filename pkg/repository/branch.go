package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/lib/pq"

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

	// SetKeyMeta records a rename, platform change or soft delete on a branch.
	SetKeyMeta(ctx context.Context, tx *gorm.DB, branchID, keyID int64, k model.Key, actor string) error

	// MetaChangedCount reports how many metadata deltas a branch carries.
	MetaChangedCount(ctx context.Context, tx *gorm.DB, branchID int64) (int, error)

	// SetStatus opens or closes a branch.
	//
	// It refuses to move a MERGED branch: a merged branch's deltas are a
	// permanent read-only record of what a merge request actually changed, and
	// reopening one would invite edits to history. Returns ErrNotFound when no
	// row moved.
	SetStatus(ctx context.Context, tx *gorm.DB, branchID int64, status string) error

	// KeyMetaForKeys returns the branch's METADATA overrides for many keys in
	// one query, so the key browser can overlay them without a lookup per row.
	KeyMetaForKeys(ctx context.Context, tx *gorm.DB, branchID int64, keyIDs []int64) (map[int64]model.Key, error)

	// Changes lists everything a branch has altered, each row flagged with
	// whether master has moved underneath it. This is the branch diff view, and
	// it uses the SAME version comparison the merge does — a diff that disagreed
	// with the merge about what conflicts would be a diff nobody could trust.
	Changes(ctx context.Context, tx *gorm.DB, branchID int64) (BranchChanges, error)

	// Summaries lists branches with their delta counts and their live merge
	// request, in ONE query.
	//
	// The counts and the request state are what the branch list actually shows,
	// and fetching them per row would be three round trips per branch for a page
	// that is rendered whole.
	Summaries(ctx context.Context, tx *gorm.DB, status string) ([]BranchSummary, error)

	// SummaryByID reads ONE branch with the same counts Summaries computes.
	//
	// The same statement scoped to one row, so a detail view and the list
	// cannot disagree — and so resolving a merge request's branch does not
	// fetch every branch in the system to find one.
	SummaryByID(ctx context.Context, tx *gorm.DB, branchID int64) (BranchSummary, error)
}

// BranchSummary is a branch plus everything the branch list displays.
//
// A separate type rather than fields on Branch, following TagUsage: the counts
// are a property of the whole delta set and only Summaries can populate them.
// Folding them into Branch would mean every Branch returned by ByName carried
// zeroes that read as "this branch changes nothing".
type BranchSummary struct {
	Branch

	ValueChanges int
	MetaChanges  int

	// MergeRequestID and MergeRequestStatus describe the LIVE request, if any.
	// Nil means the branch has no request open — which is different from having
	// one that was rejected.
	MergeRequestID     *int64
	MergeRequestStatus string
}

// BranchValueChange is one value delta, alongside what master now holds.
type BranchValueChange struct {
	KeyID      int64
	KeyName    string
	LocaleID   int16
	LocaleCode string

	// Value is the branch's value; meaningful only when Removed is false. The
	// branch's three states are the same three as master's: a tombstone
	// (Removed), a deliberate "", and text.
	Value   string
	Removed bool

	// MasterValue and MasterFound carry master's side with the same
	// distinction. MasterFound false means master has no row at all.
	MasterValue string
	MasterFound bool

	BaseMasterVersion int
	MasterVersion     int

	// Conflict is exactly the merge's rule: master moved since this branch
	// first touched the pair.
	Conflict bool

	UpdatedBy string
	UpdatedAt time.Time
}

// BranchMetaChange is one key-metadata delta.
type BranchMetaChange struct {
	// KeyID always names a real key. A key CREATED on this branch has one too:
	// it is a `keys` row from the moment it is created, held at status 'draft'
	// until the merge promotes it.
	KeyID int64

	Name        string
	Description string
	Status      string

	// MasterName and MasterStatus are master's current values. For a key
	// created on this branch MasterStatus is 'draft', which is what marks the
	// row as an introduction rather than an edit.
	MasterName   string
	MasterStatus string

	BaseMasterVersion int
	MasterVersion     int
	Conflict          bool

	UpdatedBy string
	UpdatedAt time.Time
}

// BranchChanges is a branch's complete diff against master.
type BranchChanges struct {
	Values []BranchValueChange
	Meta   []BranchMetaChange
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

// ErrBranchNameTaken is returned when Create hits branches_name_unique.
//
// An exported sentinel, following ErrTagNameTaken: the portal turns it into a
// 409 with a body a human can act on, rather than a 500 carrying a driver
// message.
var ErrBranchNameTaken = errors.New("a branch with that name already exists")

// createBranchSQL uses DO NOTHING so a taken name comes back as a missing row
// rather than a driver error to pattern-match — the same move as createTagSQL,
// and race-free in a way a pre-check is not.
const createBranchSQL = `
INSERT INTO branches (name, description, created_by)
VALUES ($1, $2, $3)
ON CONFLICT (name) DO NOTHING
RETURNING ` + branchColumns

func (r *branchRepository) Create(ctx context.Context, tx *gorm.DB, name, description, createdBy string) (Branch, error) {
	row := r.db(ctx, tx).Raw(createBranchSQL, name, description, createdBy).Row()

	b, err := scanBranch(row)
	switch {
	case err == nil:
		return b, nil
	case isNoRows(err):
		return b, fmt.Errorf("create branch %q: %w", name, ErrBranchNameTaken)
	default:
		return b, fmt.Errorf("create branch %q: %w", name, err)
	}
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

// setKeyMetaSQL writes a metadata delta for a key.
//
// "A key", not "an existing master key": since V1.07 a key created on a branch
// also has a real `keys` row from the moment it is created — a draft one — so
// this single statement serves both a rename of a master key and the delta that
// will promote a branch-created draft. The ON CONFLICT target needs no
// predicate now that key_id is NOT NULL and the index is no longer partial.
//
// base_master_version is anchored on keys.version, and — exactly as for value
// deltas — is captured only on the first touch. Refreshing it would let a
// branch silently adopt master's newer metadata as its base, and a genuine
// rename/rename race would then look clean.
const setKeyMetaSQL = `
INSERT INTO branch_keys
    (branch_id, key_id, name, description, platforms, android_name, ios_name,
     status, base_master_version, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
        COALESCE((SELECT version FROM keys WHERE id = $2), 0), $9)
ON CONFLICT (branch_id, key_id) DO UPDATE SET
    name         = EXCLUDED.name,
    description  = EXCLUDED.description,
    platforms    = EXCLUDED.platforms,
    android_name = EXCLUDED.android_name,
    ios_name     = EXCLUDED.ios_name,
    status       = EXCLUDED.status,
    -- deliberately NOT updated, same reasoning as branch_translations
    updated_by   = EXCLUDED.updated_by,
    updated_at   = now()`

// SetKeyMeta records a rename, platform change or soft delete on a branch.
func (r *branchRepository) SetKeyMeta(
	ctx context.Context, tx *gorm.DB, branchID, keyID int64, k model.Key, actor string,
) error {
	platforms := make([]string, len(k.Platforms))
	for i, p := range k.Platforms {
		platforms[i] = string(p)
	}
	if k.Status == "" {
		k.Status = model.KeyStatusActive
	}

	db := r.db(ctx, tx)
	err := db.Exec(setKeyMetaSQL, branchID, keyID, k.Name, k.Description,
		pq.Array(platforms), k.AndroidName, k.IOSName, string(k.Status), actor).Error
	if err != nil {
		return fmt.Errorf("set branch key meta for %d: %w", keyID, err)
	}
	return r.touch(ctx, db, branchID)
}

// MetaChangedCount reports how many metadata deltas a branch carries.
func (r *branchRepository) MetaChangedCount(ctx context.Context, tx *gorm.DB, branchID int64) (int, error) {
	var n int
	row := r.db(ctx, tx).Raw(
		`SELECT count(*) FROM branch_keys WHERE branch_id = ?`, branchID).Row()
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count branch key deltas: %w", err)
	}
	return n, nil
}

func (r *branchRepository) SetStatus(ctx context.Context, tx *gorm.DB, branchID int64, status string) error {
	// status <> 'merged' in the predicate, not a check in a service: a merged
	// branch's deltas are the permanent record of what a merge actually applied,
	// and reopening one would invite edits to history. Enforcing it in the
	// statement means no caller can forget.
	res := r.db(ctx, tx).Exec(
		`UPDATE branches SET status = $2 WHERE id = $1 AND status <> 'merged'`,
		branchID, status)
	if res.Error != nil {
		return fmt.Errorf("set branch %d status to %q: %w", branchID, status, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("branch %d is missing or already merged: %w", branchID, ErrNotFound)
	}
	return nil
}

const keyMetaForKeysSQL = `
SELECT key_id, name, description, platforms, android_name, ios_name, status
  FROM branch_keys
 WHERE branch_id = $1 AND key_id = ANY($2::bigint[])`

func (r *branchRepository) KeyMetaForKeys(
	ctx context.Context, tx *gorm.DB, branchID int64, keyIDs []int64,
) (map[int64]model.Key, error) {
	out := make(map[int64]model.Key, len(keyIDs))
	if len(keyIDs) == 0 || branchID == 0 {
		// Master has no deltas by definition, and an empty page should cost no
		// query at all.
		return out, nil
	}

	rows, err := r.db(ctx, tx).Raw(keyMetaForKeysSQL, branchID, pq.Array(keyIDs)).Rows()
	if err != nil {
		return nil, fmt.Errorf("read branch key metadata: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			k           model.Key
			platforms   []string
			status      string
			androidName sql.NullString
			iosName     sql.NullString
		)
		if err := rows.Scan(&k.ID, &k.Name, &k.Description, pq.Array(&platforms),
			&androidName, &iosName, &status); err != nil {
			return nil, fmt.Errorf("scan branch key metadata: %w", err)
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
		out[k.ID] = k
	}
	return out, rows.Err()
}

// branchValueChangesSQL is the branch diff, value side.
//
// The conflict expression is character-for-character conflictsSQL's:
//
//	COALESCE(t.version, 0) <> bt.base_master_version
//
// It is repeated rather than shared because the two statements select different
// row sets — the diff shows everything, the conflict query shows only the
// conflicting rows — but if one of them is ever changed, the other must change
// with it. A diff that disagreed with the merge about what conflicts is a diff
// nobody could act on.
const branchValueChangesSQL = `
SELECT bt.key_id, k.name, bt.locale_id, l.code,
       COALESCE(bt.value, '')                       AS value,
       bt.is_removed,
       COALESCE(t.value, '')                        AS master_value,
       (t.key_id IS NOT NULL)                       AS master_found,
       bt.base_master_version,
       COALESCE(t.version, 0)                       AS master_version,
       (COALESCE(t.version, 0) <> bt.base_master_version) AS conflict,
       bt.updated_by, bt.updated_at
  FROM branch_translations bt
  JOIN keys    k ON k.id = bt.key_id
  JOIN locales l ON l.id = bt.locale_id
  LEFT JOIN translations t
    ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
 WHERE bt.branch_id = $1
 ORDER BY k.sort_index, l.sort_order`

// branchMetaChangesSQL is the branch diff, metadata side.
//
// A plain JOIN since V1.07: every branch_keys row names a real key, including
// one created on this branch. A key the branch created shows up here with
// master_status = 'draft' — which is precisely how a reviewer tells "this
// branch introduces a new key" from "this branch renames an existing one",
// without the diff needing a flag of its own.
const branchMetaChangesSQL = `
SELECT bk.key_id, bk.name, bk.description, bk.status,
       k.name   AS master_name,
       k.status AS master_status,
       bk.base_master_version,
       k.version AS master_version,
       (k.version <> bk.base_master_version) AS conflict,
       bk.updated_by, bk.updated_at
  FROM branch_keys bk
  JOIN keys k ON k.id = bk.key_id
 WHERE bk.branch_id = $1
 ORDER BY bk.name`

func (r *branchRepository) Changes(ctx context.Context, tx *gorm.DB, branchID int64) (BranchChanges, error) {
	var out BranchChanges

	rows, err := r.db(ctx, tx).Raw(branchValueChangesSQL, branchID).Rows()
	if err != nil {
		return out, fmt.Errorf("read branch %d value changes: %w", branchID, err)
	}
	for rows.Next() {
		var c BranchValueChange
		if err := rows.Scan(&c.KeyID, &c.KeyName, &c.LocaleID, &c.LocaleCode,
			&c.Value, &c.Removed, &c.MasterValue, &c.MasterFound,
			&c.BaseMasterVersion, &c.MasterVersion, &c.Conflict,
			&c.UpdatedBy, &c.UpdatedAt); err != nil {
			rows.Close()
			return out, fmt.Errorf("scan branch value change: %w", err)
		}
		if c.Removed {
			// A tombstone carries no value; the column is NULL and the COALESCE
			// above turned it into '', which would read as a deliberate blank.
			c.Value = ""
		}
		out.Values = append(out.Values, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, fmt.Errorf("read branch %d value changes: %w", branchID, err)
	}
	rows.Close()

	metaRows, err := r.db(ctx, tx).Raw(branchMetaChangesSQL, branchID).Rows()
	if err != nil {
		return out, fmt.Errorf("read branch %d metadata changes: %w", branchID, err)
	}
	defer metaRows.Close()

	for metaRows.Next() {
		var c BranchMetaChange
		if err := metaRows.Scan(&c.KeyID, &c.Name, &c.Description, &c.Status,
			&c.MasterName, &c.MasterStatus, &c.BaseMasterVersion,
			&c.MasterVersion, &c.Conflict, &c.UpdatedBy, &c.UpdatedAt); err != nil {
			return out, fmt.Errorf("scan branch metadata change: %w", err)
		}
		out.Meta = append(out.Meta, c)
	}
	return out, metaRows.Err()
}

// branchSummarySelect folds three per-branch questions into one statement.
//
// The subqueries are correlated scalar selects rather than joins with GROUP BY,
// because a branch with no deltas must still appear — an inner join would hide
// exactly the branch somebody just created and is looking for.
//
// The merge-request subquery repeats ByBranch's "live" predicate: terminal
// states are excluded so a branch whose request was rejected reads as having
// none, which is what lets a fresh request be opened against it.
//
// Shared between Summaries and SummaryByID, which differ only in their WHERE —
// two copies would be two definitions of "what a branch summary means", and
// they would drift.
const branchSummarySelect = `
SELECT b.id, b.name, b.description, b.status, b.created_by, b.created_at,
       b.last_edited_at, b.merged_at,
       (SELECT count(*) FROM branch_translations bt WHERE bt.branch_id = b.id) AS value_changes,
       (SELECT count(*) FROM branch_keys bk        WHERE bk.branch_id = b.id) AS meta_changes,
       mr.id, COALESCE(mr.status, '')
  FROM branches b
  LEFT JOIN merge_requests mr
    ON mr.branch_id = b.id
   AND mr.status NOT IN ('merged', 'closed', 'rejected')`

const branchSummariesSQL = branchSummarySelect + `
 WHERE ($1 = '' OR b.status = $1)
 ORDER BY b.created_at DESC`

const branchSummaryByIDSQL = branchSummarySelect + `
 WHERE b.id = $1`

func scanBranchSummary(row interface{ Scan(...interface{}) error }) (BranchSummary, error) {
	var s BranchSummary
	err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Status,
		&s.CreatedBy, &s.CreatedAt, &s.LastEditedAt, &s.MergedAt,
		&s.ValueChanges, &s.MetaChanges,
		&s.MergeRequestID, &s.MergeRequestStatus)
	return s, err
}

func (r *branchRepository) Summaries(ctx context.Context, tx *gorm.DB, status string) ([]BranchSummary, error) {
	rows, err := r.db(ctx, tx).Raw(branchSummariesSQL, status).Rows()
	if err != nil {
		return nil, fmt.Errorf("list branch summaries: %w", err)
	}
	defer rows.Close()

	var out []BranchSummary
	for rows.Next() {
		s, err := scanBranchSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("scan branch summary: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *branchRepository) SummaryByID(ctx context.Context, tx *gorm.DB, branchID int64) (BranchSummary, error) {
	row := r.db(ctx, tx).Raw(branchSummaryByIDSQL, branchID).Row()

	s, err := scanBranchSummary(row)
	switch {
	case err == nil:
		return s, nil
	case isNoRows(err):
		return s, fmt.Errorf("branch %d: %w", branchID, ErrNotFound)
	default:
		return s, fmt.Errorf("read branch %d summary: %w", branchID, err)
	}
}
