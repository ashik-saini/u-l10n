package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
)

// MergeRequest is a request to fold a branch's deltas into master.
type MergeRequest struct {
	ID         int64
	BranchID   int64
	Title      string
	Status     string
	CreatedBy  string
	CreatedAt  time.Time
	ApprovedBy *string
	ApprovedAt *time.Time
	MergedAt   *time.Time
}

const (
	MRStatusOpen             = "open"
	MRStatusApproved         = "approved"
	MRStatusChangesRequested = "changes_requested"
	MRStatusRejected         = "rejected"
	MRStatusMerged           = "merged"
	MRStatusClosed           = "closed"
)

// Conflict is one (key, locale) whose master row moved since the branch first
// touched it.
type Conflict struct {
	KeyID      int64
	LocaleID   int16
	KeyName    string
	LocaleCode string

	// Mine is the branch's value. Valid only when MineRemoved is false.
	Mine        string
	MineRemoved bool
	// Theirs is master's current value; TheirsFound distinguishes a master row
	// holding "" from no master row at all.
	Theirs      string
	TheirsFound bool

	BaseMasterVersion int
	MasterVersion     int

	// Resolution is "mine", "master", or empty when unresolved.
	Resolution string
}

// Resolved reports whether a human has already decided this conflict.
func (c Conflict) Resolved() bool { return c.Resolution != "" }

// MergeRequestRepository owns merge requests, their events and conflicts.
type MergeRequestRepository interface {
	Create(ctx context.Context, tx *gorm.DB, branchID int64, title, createdBy string) (MergeRequest, error)
	ByBranch(ctx context.Context, tx *gorm.DB, branchID int64) (MergeRequest, error)

	// Approve marks the request approved, recording who and when. The merge
	// transaction later re-checks that the branch has not been edited since.
	Approve(ctx context.Context, tx *gorm.DB, mrID int64, approver string) error

	// InvalidateApprovalIfEdited flips an approved request back to open when
	// the branch changed after approval, writing a system event.
	//
	// It is what stops a reviewer approving one diff and a different one being
	// merged. Called on every branch write, and again defensively inside the
	// merge transaction — the second call is the one that actually protects,
	// because anything checked before the transaction is already stale.
	InvalidateApprovalIfEdited(ctx context.Context, tx *gorm.DB, branchID int64) (invalidated bool, err error)

	SetStatus(ctx context.Context, tx *gorm.DB, mrID int64, status, actor, comment string) error

	// Conflicts computes value conflicts for a branch, with any stored
	// resolutions attached.
	Conflicts(ctx context.Context, tx *gorm.DB, mrID, branchID int64) ([]Conflict, error)

	Resolve(ctx context.Context, tx *gorm.DB, mrID, keyID int64, localeID int16, resolution, actor string) error

	// MetaConflicts computes key-metadata conflicts, anchored on keys.version.
	MetaConflicts(ctx context.Context, tx *gorm.DB, mrID, branchID int64) ([]MetaConflict, error)

	// NameCollisions finds branch names an active master key already holds.
	NameCollisions(ctx context.Context, tx *gorm.DB, branchID int64) ([]NameCollision, error)

	// ApplyKeyMeta folds metadata deltas into master.
	ApplyKeyMeta(ctx context.Context, tx *gorm.DB, mrID, branchID int64) (int, error)

	// ByID reads one request in any state, including the terminal ones. The
	// portal has to be able to show a rejected request; ByBranch deliberately
	// cannot.
	ByID(ctx context.Context, tx *gorm.DB, mrID int64) (MergeRequest, error)

	// List returns requests with their branch name, newest first. An empty
	// status means every state.
	List(ctx context.Context, tx *gorm.DB, status string) ([]MergeRequestListing, error)

	// Events reads the request's workflow timeline, oldest first — it reads as
	// a narrative, and a narrative told backwards is one nobody follows.
	Events(ctx context.Context, tx *gorm.DB, mrID int64) ([]MergeRequestEvent, error)

	// ResolveMeta stores a decision for a key-METADATA conflict, which has no
	// locale dimension.
	//
	// A separate method rather than a nullable locale on Resolve: the unique
	// index keys on COALESCE(locale_id, -1), so the two cases target different
	// conflict rows, and a single method taking a *int16 would let a caller
	// silently record a metadata decision against locale id 0.
	ResolveMeta(ctx context.Context, tx *gorm.DB, mrID, keyID int64, resolution, actor string) error
}

// ErrLiveMergeRequestExists is returned when a transition would produce a
// second live request for one branch.
//
// idx_merge_requests_one_live_per_branch is partial over the non-terminal
// states, so reopening a rejected request while another is open violates it.
// That is a 409 a human must resolve, not a 500.
var ErrLiveMergeRequestExists = errors.New("this branch already has a live merge request")

// MergeRequestListing is a request plus the branch it belongs to.
//
// The branch NAME rather than only its id, because every merge endpoint keys on
// the name and a list that forced a lookup per row to render a link would be one
// round trip per row.
type MergeRequestListing struct {
	MergeRequest
	BranchName   string
	BranchStatus string
}

// MergeRequestEvent is one entry in the workflow timeline.
type MergeRequestEvent struct {
	ID    int64
	Event string
	// Comment carries a reviewer's reason. Empty is normal — an approval needs
	// none — so it is not a pointer.
	Comment string
	// Actor is 'system' for automatic transitions, such as an approval
	// invalidated by a later branch edit.
	Actor     string
	CreatedAt time.Time
}

type mergeRequestRepository struct{ base }

func ProvideMergeRequestRepository(connector database.GORMConnector) MergeRequestRepository {
	return &mergeRequestRepository{base{connector: connector}}
}

const mrColumns = `id, branch_id, title, status, created_by, created_at, approved_by, approved_at, merged_at`

func scanMR(row interface{ Scan(...interface{}) error }) (MergeRequest, error) {
	var m MergeRequest
	err := row.Scan(&m.ID, &m.BranchID, &m.Title, &m.Status, &m.CreatedBy,
		&m.CreatedAt, &m.ApprovedBy, &m.ApprovedAt, &m.MergedAt)
	return m, err
}

func (r *mergeRequestRepository) Create(
	ctx context.Context, tx *gorm.DB, branchID int64, title, createdBy string,
) (MergeRequest, error) {
	db := r.db(ctx, tx)

	row := db.Raw(`
		INSERT INTO merge_requests (branch_id, title, created_by)
		VALUES ($1, $2, $3) RETURNING `+mrColumns, branchID, title, createdBy).Row()

	m, err := scanMR(row)
	if err != nil {
		// The partial unique index permits only one LIVE request per branch,
		// so a duplicate here means one is already open. Classified rather than
		// passed through: two people opening a request for the same branch is a
		// race a human resolves, not a fault an engineer investigates.
		if isUniqueViolation(err) {
			return m, fmt.Errorf("create merge request for branch %d: %w",
				branchID, ErrLiveMergeRequestExists)
		}
		return m, fmt.Errorf("create merge request for branch %d: %w", branchID, err)
	}

	if err := r.event(ctx, db, m.ID, "created", createdBy, ""); err != nil {
		return m, err
	}
	return m, nil
}

func (r *mergeRequestRepository) ByBranch(ctx context.Context, tx *gorm.DB, branchID int64) (MergeRequest, error) {
	row := r.db(ctx, tx).Raw(`
		SELECT `+mrColumns+` FROM merge_requests
		 WHERE branch_id = $1 AND status NOT IN ('merged','closed','rejected')`, branchID).Row()

	m, err := scanMR(row)
	switch {
	case err == nil:
		return m, nil
	case isNoRows(err):
		return m, fmt.Errorf("no live merge request for branch %d: %w", branchID, ErrNotFound)
	default:
		return m, err
	}
}

func (r *mergeRequestRepository) Approve(ctx context.Context, tx *gorm.DB, mrID int64, approver string) error {
	db := r.db(ctx, tx)

	res := db.Exec(`
		UPDATE merge_requests
		   SET status = 'approved', approved_by = $1, approved_at = now()
		 WHERE id = $2 AND status IN ('open','changes_requested')`, approver, mrID)
	if res.Error != nil {
		return fmt.Errorf("approve merge request %d: %w", mrID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("merge request %d is not in an approvable state: %w", mrID, ErrNotFound)
	}
	return r.event(ctx, db, mrID, "approved", approver, "")
}

// invalidateApprovalSQL re-opens an approved request whose branch has been
// edited since the approval.
//
// The comparison is on branches.last_edited_at, which every delta write bumps.
// A NULL last_edited_at cannot be newer, so a branch that has not been touched
// since approval is unaffected.
const invalidateApprovalSQL = `
UPDATE merge_requests mr
   SET status = 'open', approved_by = NULL, approved_at = NULL
  FROM branches b
 WHERE mr.branch_id = b.id
   AND mr.branch_id = $1
   AND mr.status = 'approved'
   AND b.last_edited_at IS NOT NULL
   AND b.last_edited_at > mr.approved_at
RETURNING mr.id`

func (r *mergeRequestRepository) InvalidateApprovalIfEdited(
	ctx context.Context, tx *gorm.DB, branchID int64,
) (bool, error) {
	db := r.db(ctx, tx)

	var mrID int64
	row := db.Raw(invalidateApprovalSQL, branchID).Row()
	switch err := row.Scan(&mrID); {
	case err == nil:
	case isNoRows(err):
		// Nothing to invalidate: not approved, or not edited since.
		return false, nil
	default:
		return false, fmt.Errorf("invalidate approval for branch %d: %w", branchID, err)
	}

	// 'system' as the actor, because no human performed this.
	if err := r.event(ctx, db, mrID, "approval_invalidated", "system",
		"branch was edited after approval"); err != nil {
		return true, err
	}
	return true, nil
}

func (r *mergeRequestRepository) SetStatus(
	ctx context.Context, tx *gorm.DB, mrID int64, status, actor, comment string,
) error {
	db := r.db(ctx, tx)

	if err := db.Exec(
		`UPDATE merge_requests SET status = $1 WHERE id = $2`, status, mrID).Error; err != nil {
		// Moving OUT of a terminal state can collide with
		// idx_merge_requests_one_live_per_branch, which permits one live request
		// per branch. That is a state a human must resolve, so it is classified
		// here rather than escaping as a driver error and becoming a 500.
		if isUniqueViolation(err) {
			return fmt.Errorf("reopen merge request %d: %w", mrID, ErrLiveMergeRequestExists)
		}
		return fmt.Errorf("set merge request %d status: %w", mrID, err)
	}
	return r.event(ctx, db, mrID, statusEvent(status), actor, comment)
}

func (r *mergeRequestRepository) ByID(ctx context.Context, tx *gorm.DB, mrID int64) (MergeRequest, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+mrColumns+` FROM merge_requests WHERE id = $1`, mrID).Row()

	m, err := scanMR(row)
	switch {
	case err == nil:
		return m, nil
	case isNoRows(err):
		return m, fmt.Errorf("merge request %d: %w", mrID, ErrNotFound)
	default:
		return m, fmt.Errorf("read merge request %d: %w", mrID, err)
	}
}

const listMergeRequestsSQL = `
SELECT mr.id, mr.branch_id, mr.title, mr.status, mr.created_by, mr.created_at,
       mr.approved_by, mr.approved_at, mr.merged_at,
       b.name, b.status
  FROM merge_requests mr
  JOIN branches b ON b.id = mr.branch_id
 WHERE ($1 = '' OR mr.status = $1)
 ORDER BY mr.created_at DESC, mr.id DESC`

func (r *mergeRequestRepository) List(
	ctx context.Context, tx *gorm.DB, status string,
) ([]MergeRequestListing, error) {
	rows, err := r.db(ctx, tx).Raw(listMergeRequestsSQL, status).Rows()
	if err != nil {
		return nil, fmt.Errorf("list merge requests: %w", err)
	}
	defer rows.Close()

	var out []MergeRequestListing
	for rows.Next() {
		var l MergeRequestListing
		if err := rows.Scan(&l.ID, &l.BranchID, &l.Title, &l.Status, &l.CreatedBy,
			&l.CreatedAt, &l.ApprovedBy, &l.ApprovedAt, &l.MergedAt,
			&l.BranchName, &l.BranchStatus); err != nil {
			return nil, fmt.Errorf("scan merge request: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *mergeRequestRepository) Events(
	ctx context.Context, tx *gorm.DB, mrID int64,
) ([]MergeRequestEvent, error) {
	rows, err := r.db(ctx, tx).Raw(`
		SELECT id, event, comment, actor, created_at
		  FROM merge_request_events
		 WHERE merge_request_id = $1
		 ORDER BY created_at, id`, mrID).Rows()
	if err != nil {
		return nil, fmt.Errorf("read merge request %d events: %w", mrID, err)
	}
	defer rows.Close()

	var out []MergeRequestEvent
	for rows.Next() {
		var e MergeRequestEvent
		if err := rows.Scan(&e.ID, &e.Event, &e.Comment, &e.Actor, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan merge request event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// resolveMetaSQL stores a decision for a metadata conflict.
//
// locale_id is NULL, and the conflict target must therefore match the unique
// index's COALESCE(locale_id, -1) expression exactly — a plain (merge_request_id,
// key_id, locale_id) target would not collide with an existing NULL row and the
// same conflict could accumulate contradictory decisions.
const resolveMetaSQL = `
INSERT INTO merge_conflict_resolutions
    (merge_request_id, key_id, locale_id, resolution, resolved_by)
VALUES ($1, $2, NULL, $3, $4)
ON CONFLICT (merge_request_id, key_id, COALESCE(locale_id, -1)) DO UPDATE SET
    resolution  = EXCLUDED.resolution,
    resolved_by = EXCLUDED.resolved_by,
    resolved_at = now()`

func (r *mergeRequestRepository) ResolveMeta(
	ctx context.Context, tx *gorm.DB, mrID, keyID int64, resolution, actor string,
) error {
	switch resolution {
	case "mine", "master":
	default:
		return fmt.Errorf("invalid resolution %q: want mine or master", resolution)
	}

	if err := r.db(ctx, tx).Exec(resolveMetaSQL, mrID, keyID, resolution, actor).Error; err != nil {
		return fmt.Errorf("store metadata resolution for key %d: %w", keyID, err)
	}
	return nil
}

func statusEvent(status string) string {
	switch status {
	case MRStatusChangesRequested:
		return "changes_requested"
	case MRStatusRejected:
		return "rejected"
	case MRStatusMerged:
		return "merged"
	case MRStatusClosed:
		return "closed"
	case MRStatusOpen:
		return "reopened"
	default:
		return "created"
	}
}

func (r *mergeRequestRepository) event(
	ctx context.Context, db *gorm.DB, mrID int64, event, actor, comment string,
) error {
	if err := db.Exec(`
		INSERT INTO merge_request_events (merge_request_id, event, comment, actor)
		VALUES ($1, $2, $3, $4)`, mrID, event, comment, actor).Error; err != nil {
		return fmt.Errorf("record merge request event %q: %w", event, err)
	}
	return nil
}

// conflictsSQL is the entire value-conflict rule.
//
//	COALESCE(t.version, 0) <> bt.base_master_version
//
// One integer comparison covering every race:
//
//	edited v7   / master edited to v8      7 <> 8  conflict
//	edited v7   / master untouched         7 <> 7  clean
//	created (0) / master gained a row      0 <> 1  conflict
//	created (0) / master still empty       0 <> 0  clean
//	removed v7  / master edited to v8      7 <> 8  conflict
//
// The alternative is four separate detection paths, and the fourth is always
// the buggy one.
//
// Stored resolutions are LEFT JOINed so the caller sees in one pass which
// conflicts a human has already decided.
const conflictsSQL = `
SELECT bt.key_id, bt.locale_id, k.name, l.code,
       COALESCE(bt.value, '')            AS mine,
       bt.is_removed,
       COALESCE(t.value, '')             AS theirs,
       (t.key_id IS NOT NULL)            AS theirs_found,
       bt.base_master_version,
       COALESCE(t.version, 0)            AS master_version,
       COALESCE(mcr.resolution, '')      AS resolution
  FROM branch_translations bt
  JOIN keys    k ON k.id = bt.key_id
  JOIN locales l ON l.id = bt.locale_id
  LEFT JOIN translations t
    ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
  LEFT JOIN merge_conflict_resolutions mcr
    ON mcr.merge_request_id = $2
   AND mcr.key_id = bt.key_id
   AND COALESCE(mcr.locale_id, -1) = bt.locale_id
 WHERE bt.branch_id = $1
   AND COALESCE(t.version, 0) <> bt.base_master_version
 ORDER BY k.sort_index, l.sort_order`

func (r *mergeRequestRepository) Conflicts(
	ctx context.Context, tx *gorm.DB, mrID, branchID int64,
) ([]Conflict, error) {
	rows, err := r.db(ctx, tx).Raw(conflictsSQL, branchID, mrID).Rows()
	if err != nil {
		return nil, fmt.Errorf("compute conflicts for branch %d: %w", branchID, err)
	}
	defer rows.Close()

	var out []Conflict
	for rows.Next() {
		var c Conflict
		if err := rows.Scan(&c.KeyID, &c.LocaleID, &c.KeyName, &c.LocaleCode,
			&c.Mine, &c.MineRemoved, &c.Theirs, &c.TheirsFound,
			&c.BaseMasterVersion, &c.MasterVersion, &c.Resolution); err != nil {
			return nil, fmt.Errorf("scan conflict: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *mergeRequestRepository) Resolve(
	ctx context.Context, tx *gorm.DB, mrID, keyID int64, localeID int16, resolution, actor string,
) error {
	switch resolution {
	case "mine", "master":
	default:
		return fmt.Errorf("invalid resolution %q: want mine or master", resolution)
	}

	// The unique index uses COALESCE(locale_id, -1), so the conflict target
	// must match it exactly.
	err := r.db(ctx, tx).Exec(`
		INSERT INTO merge_conflict_resolutions
		    (merge_request_id, key_id, locale_id, resolution, resolved_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (merge_request_id, key_id, COALESCE(locale_id, -1)) DO UPDATE SET
		    resolution  = EXCLUDED.resolution,
		    resolved_by = EXCLUDED.resolved_by,
		    resolved_at = now()`,
		mrID, keyID, localeID, resolution, actor).Error
	if err != nil {
		return fmt.Errorf("store resolution for (%d,%d): %w", keyID, localeID, err)
	}
	return nil
}

// MetaConflict is a key whose METADATA moved on master since the branch first
// touched it. Anchored on keys.version, not translations.version.
type MetaConflict struct {
	KeyID                            int64
	MineName, TheirsName             string
	MineStatus, TheirsStatus         string
	BaseMasterVersion, MasterVersion int
	Resolution                       string
}

func (c MetaConflict) Resolved() bool { return c.Resolution != "" }

// NameCollision is the THIRD conflict type, and the only one that is not a
// version comparison.
//
// A branch renames a key to — or creates one called — a name that master
// independently gained. Both sides are internally consistent; it is the union
// that is impossible, because idx_keys_name_active permits one active key per
// name. Merging regardless would fail on the unique index mid-transaction, so
// it must be detected up front and shown to a human.
//
// BranchKeyID is a plain id, not a pointer: since V1.07 every branch_keys row
// names a real key. For a key CREATED on the branch it is that key's draft row,
// which the merge promotes to active — and which is why the detection below
// cannot simply compare names and must exclude the branch's own key.
type NameCollision struct {
	BranchKeyID int64
	Name        string
	MasterKeyID int64
}

// metaConflictsSQL mirrors the value rule, anchored on keys.version.
const metaConflictsSQL = `
SELECT bk.key_id,
       bk.name, k.name,
       bk.status, k.status,
       bk.base_master_version, COALESCE(k.version, 0),
       COALESCE(mcr.resolution, '')
  FROM branch_keys bk
  JOIN keys k ON k.id = bk.key_id
  LEFT JOIN merge_conflict_resolutions mcr
    ON mcr.merge_request_id = $2
   AND mcr.key_id = bk.key_id
   AND mcr.locale_id IS NULL
 WHERE bk.branch_id = $1
   AND COALESCE(k.version, 0) <> bk.base_master_version
 ORDER BY k.sort_index`

func (r *mergeRequestRepository) MetaConflicts(
	ctx context.Context, tx *gorm.DB, mrID, branchID int64,
) ([]MetaConflict, error) {
	rows, err := r.db(ctx, tx).Raw(metaConflictsSQL, branchID, mrID).Rows()
	if err != nil {
		return nil, fmt.Errorf("compute metadata conflicts: %w", err)
	}
	defer rows.Close()

	var out []MetaConflict
	for rows.Next() {
		var c MetaConflict
		if err := rows.Scan(&c.KeyID, &c.MineName, &c.TheirsName,
			&c.MineStatus, &c.TheirsStatus,
			&c.BaseMasterVersion, &c.MasterVersion, &c.Resolution); err != nil {
			return nil, fmt.Errorf("scan metadata conflict: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nameCollisionsSQL finds branch names that an ACTIVE master key already holds.
//
// The self-match is excluded: a branch delta that keeps a key's own name is not
// colliding with itself. Only a DIFFERENT master key holding the target name is
// a real collision.
//
// This is also what catches a key CREATED on a branch whose name master gained
// independently, or which a second branch created and merged first. The
// branch's own draft row cannot match, because the join requires
// k.status = 'active' and a draft is not; the moment another branch's draft is
// promoted to active by ITS merge, this query starts reporting the collision
// and the second merge refuses loudly instead of dying on idx_keys_name_active
// halfway through applying changes.
const nameCollisionsSQL = `
SELECT bk.key_id, bk.name, k.id
  FROM branch_keys bk
  JOIN keys k ON k.name = bk.name AND k.status = 'active'
 WHERE bk.branch_id = $1
   AND bk.status = 'active'
   AND bk.key_id <> k.id
 ORDER BY bk.name`

func (r *mergeRequestRepository) NameCollisions(
	ctx context.Context, tx *gorm.DB, branchID int64,
) ([]NameCollision, error) {
	rows, err := r.db(ctx, tx).Raw(nameCollisionsSQL, branchID).Rows()
	if err != nil {
		return nil, fmt.Errorf("compute name collisions: %w", err)
	}
	defer rows.Close()

	var out []NameCollision
	for rows.Next() {
		var c NameCollision
		if err := rows.Scan(&c.BranchKeyID, &c.Name, &c.MasterKeyID); err != nil {
			return nil, fmt.Errorf("scan name collision: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// applyKeyMetaSQL folds metadata deltas into master.
//
// Applied BEFORE value deltas in the merge, because a rename must land before
// values are written against the key, and a soft delete must not be undone by a
// value write that follows it.
//
// This one UPDATE is also how a key CREATED on a branch reaches master. Such a
// key already has a `keys` row — inserted at creation with status = 'draft', so
// it is excluded from every export and every OTA bundle until it lands — and a
// branch_keys delta carrying status = 'active'. `status = bk.status` promotes
// it. There is no INSERT branch here and deliberately so: one path by which a
// key becomes visible on master is one path to get wrong.
const applyKeyMetaSQL = `
UPDATE keys k
   SET name         = bk.name,
       description  = bk.description,
       platforms    = bk.platforms,
       android_name = bk.android_name,
       ios_name     = bk.ios_name,
       status       = bk.status,
       version      = k.version + 1,
       updated_at   = now()
  FROM branch_keys bk
  LEFT JOIN merge_conflict_resolutions mcr
    ON mcr.merge_request_id = $2
   AND mcr.key_id = bk.key_id
   AND mcr.locale_id IS NULL
 WHERE bk.branch_id = $1
   AND bk.key_id = k.id
   AND COALESCE(mcr.resolution, 'mine') <> 'master'`

func (r *mergeRequestRepository) ApplyKeyMeta(
	ctx context.Context, tx *gorm.DB, mrID, branchID int64,
) (int, error) {
	res := r.db(ctx, tx).Exec(applyKeyMetaSQL, branchID, mrID)
	if res.Error != nil {
		return 0, fmt.Errorf("apply key metadata: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}
