// Package mergesvc folds a branch's copy-on-write deltas into master.
//
// This is the subtlest transaction in the service. Every step below exists
// because of a specific way the merge can silently ship the wrong copy.
package mergesvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

// mergeLockKey is the advisory-lock key that serialises ALL merges.
//
// One shared constant, deliberately: a second caller choosing a different
// integer would believe it held the lock while another merge ran concurrently.
const mergeLockKey int64 = 8_675_309

// MergeLockKey returns the advisory-lock key the merge takes, so a test — or
// any future second taker — can assert it is contending on the SAME lock. Two
// callers with different keys would each believe they held it.
func MergeLockKey() int64 { return mergeLockKey }

var (
	// ErrStaleApproval means the branch changed after it was approved, so the
	// diff a reviewer signed off is not the diff being merged.
	ErrStaleApproval = errors.New("approval is stale: the branch was edited after approval")

	// ErrNotApproved means the request never reached an approved state.
	ErrNotApproved = errors.New("merge request is not approved")

	// ErrUnresolvedConflicts carries the conflicts still awaiting a human.
	ErrUnresolvedConflicts = errors.New("unresolved conflicts")

	// ErrNameCollision means the branch and master independently claimed the
	// same key name. Unlike the other two types this is not resolvable by
	// choosing a side — one of them has to be renamed first.
	ErrNameCollision = errors.New("key name collision")

	// ErrConcurrentMasterWrite means a master row moved between the conflict
	// computation and the apply, with no human decision covering it. The
	// transaction rolls back and nothing was applied; the merge is safe to
	// retry, and the retry will surface the new conflict for resolution.
	ErrConcurrentMasterWrite = errors.New(
		"a concurrent write to master raced this merge; retry the merge")
)

// UnresolvedError reports which conflicts blocked a merge.
type UnresolvedError struct {
	Values []repository.Conflict
	Meta   []repository.MetaConflict
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("%v: %d value and %d metadata conflict(s) require a decision",
		ErrUnresolvedConflicts, len(e.Values), len(e.Meta))
}
func (e *UnresolvedError) Unwrap() error { return ErrUnresolvedConflicts }

// CollisionError reports names claimed by both sides.
type CollisionError struct {
	Collisions []repository.NameCollision
}

func (e *CollisionError) Error() string {
	return fmt.Sprintf("%v: %d name(s) already held by an active key on master",
		ErrNameCollision, len(e.Collisions))
}
func (e *CollisionError) Unwrap() error { return ErrNameCollision }

// Result describes a completed merge.
type Result struct {
	ReleaseID      int64
	ReleaseVersion int64
	ValuesApplied  int
	KeysApplied    int
	BundlesWritten int
}

// Service performs merges.
type Service struct {
	tx       database.Transactional
	branches repository.BranchRepository
	mrs      repository.MergeRequestRepository
	locales  repository.LocaleRepository
	releases repository.ReleaseRepository
	rows     repository.ExportRowReader
}

func ProvideService(
	tx database.Transactional,
	branches repository.BranchRepository,
	mrs repository.MergeRequestRepository,
	locales repository.LocaleRepository,
	releases repository.ReleaseRepository,
	rows repository.ExportRowReader,
) *Service {
	return &Service{tx: tx, branches: branches, mrs: mrs,
		locales: locales, releases: releases, rows: rows}
}

// Merge folds a branch into master and cuts a release.
//
// The ENTIRE operation is one transaction, and this service owns it. The shared
// WithTransaction helper does not nest, so no repository called from here may
// open its own — every call receives tx. If one did, the work would split
// across two transactions and the atomicity everything below depends on would
// vanish with no error raised.
func (s *Service) Merge(ctx context.Context, branchName, actor string) (*Result, error) {
	var result *Result

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		// STEP 1 — serialise all merges.
		//
		// Postgres defaults to READ COMMITTED, which does NOT stop two
		// concurrent merges from each reading a consistent-looking world and
		// both writing. The _xact_ variant releases on commit OR rollback, so a
		// crashing process cannot leak the lock. Merges are rare and take
		// milliseconds; serialising them globally costs nothing and removes an
		// entire class of bug.
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, mergeLockKey).Error; err != nil {
			return fmt.Errorf("acquire merge lock: %w", err)
		}

		branch, err := s.branches.ByName(ctx, tx, branchName)
		if err != nil {
			return err
		}

		// STEP 2 — re-validate INSIDE the transaction.
		//
		// The caller almost certainly checked "is this approved?" before
		// getting here. That check was stale the moment it returned. Anything
		// acted upon must be re-read inside the transaction that acts — this is
		// TOCTOU, and it is the difference between a guard and a decoration.
		invalidated, err := s.mrs.InvalidateApprovalIfEdited(ctx, tx, branch.ID)
		if err != nil {
			return err
		}
		if invalidated {
			return ErrStaleApproval
		}

		mr, err := s.mrs.ByBranch(ctx, tx, branch.ID)
		if err != nil {
			return err
		}
		if mr.Status != repository.MRStatusApproved {
			return fmt.Errorf("%w (status %q)", ErrNotApproved, mr.Status)
		}

		// STEP 3 — lock the affected master rows in a DETERMINISTIC order.
		//
		// Two transactions locking the same rows in different orders deadlock.
		// ORDER BY key_id, locale_id makes that impossible between merges, and
		// also linearises against admin direct-edits to master.
		if err := tx.Exec(`
			SELECT t.key_id
			  FROM translations t
			  JOIN branch_translations bt
			    ON bt.key_id = t.key_id AND bt.locale_id = t.locale_id
			 WHERE bt.branch_id = ?
			 ORDER BY t.key_id, t.locale_id
			   FOR UPDATE`, branch.ID).Error; err != nil {
			return fmt.Errorf("lock affected rows: %w", err)
		}

		// Also lock every `keys` row the branch touches. The lock above only
		// reaches translations rows that EXIST — a pair the branch created has
		// no master row to lock, and a metadata delta locks nothing at all —
		// so a concurrent rename or key edit could otherwise commit between
		// STEP 4 and STEP 5 and be silently overwritten. Same deterministic
		// order, same reasoning.
		if err := tx.Exec(`
			SELECT k.id
			  FROM keys k
			  JOIN branch_keys bk ON bk.key_id = k.id
			 WHERE bk.branch_id = ?
			 ORDER BY k.id
			   FOR UPDATE`, branch.ID).Error; err != nil {
			return fmt.Errorf("lock affected keys: %w", err)
		}

		// STEP 4 — every conflict must already have a human decision.
		//
		// Auto-resolving means silently choosing one person's words over
		// another's. Return the list and make someone choose.
		conflicts, err := s.mrs.Conflicts(ctx, tx, mr.ID, branch.ID)
		if err != nil {
			return err
		}
		var unresolvedValues []repository.Conflict
		for _, c := range conflicts {
			if !c.Resolved() {
				unresolvedValues = append(unresolvedValues, c)
			}
		}

		// Metadata conflicts are a SECOND type, anchored on keys.version rather
		// than translations.version. A branch that renames a key master also
		// renamed is just as much a conflict as two edits to one value.
		metaConflicts, err := s.mrs.MetaConflicts(ctx, tx, mr.ID, branch.ID)
		if err != nil {
			return err
		}
		var unresolvedMeta []repository.MetaConflict
		for _, c := range metaConflicts {
			if !c.Resolved() {
				unresolvedMeta = append(unresolvedMeta, c)
			}
		}

		if len(unresolvedValues) > 0 || len(unresolvedMeta) > 0 {
			return &UnresolvedError{Values: unresolvedValues, Meta: unresolvedMeta}
		}

		// The THIRD type, and the only one that is not a version comparison:
		// both sides independently claimed a name. Choosing a side cannot fix
		// it — one of them must be renamed — so it is rejected rather than
		// offered for resolution. Detecting it here also stops the merge dying
		// on idx_keys_name_active halfway through applying changes.
		collisions, err := s.mrs.NameCollisions(ctx, tx, branch.ID)
		if err != nil {
			return err
		}
		if len(collisions) > 0 {
			return &CollisionError{Collisions: collisions}
		}

		// STEP 5 — apply metadata FIRST, then values.
		//
		// Order matters: a rename must land before values are written against
		// the key, and a soft delete must not be silently undone by a value
		// write that follows it.
		//
		// Every apply statement re-checks the version rule in its own
		// predicate — a delta lands only while master is still at the delta's
		// base_master_version, or a human explicitly chose 'mine'. The locks
		// in STEP 3 make a mid-merge master write nearly impossible, but the
		// guard here is what makes it IMPOSSIBLE to overwrite one silently: a
		// delta blocked by a version the conflict check never saw fails the
		// whole merge with ErrConcurrentMasterWrite and rolls everything back.
		keysApplied, keysBlocked, err := s.mrs.ApplyKeyMeta(ctx, tx, mr.ID, branch.ID, actor)
		if err != nil {
			return err
		}
		if keysBlocked > 0 {
			return fmt.Errorf("%w: %d key metadata delta(s) no longer match master",
				ErrConcurrentMasterWrite, keysBlocked)
		}

		applied, err := s.applyDeltas(ctx, tx, mr.ID, branch.ID, actor)
		if err != nil {
			return err
		}

		// STEP 6 — cut a release and materialise every bundle, in the SAME
		// transaction.
		//
		// This is the keystone. Doing it here means the export endpoint and the
		// OTA endpoint are afterwards read-only handlers over identical
		// precomputed rows — they cannot disagree, because there is nothing
		// left to disagree about.
		release, err := s.releases.Create(ctx, tx, "merge", &mr.ID, actor)
		if err != nil {
			return err
		}

		locales, err := s.locales.List(ctx, tx)
		if err != nil {
			return err
		}
		for _, locale := range locales {
			rows, err := s.rows.ForExport(ctx, tx, locale.ID, model.PlatformFlutter)
			if err != nil {
				return err
			}
			if err := s.releases.MaterialiseBundle(ctx, tx, release.ID, locale, rows); err != nil {
				return err
			}
		}

		// STEP 7 — close out. Merged branches KEEP their deltas as a permanent
		// read-only record of what this request actually changed.
		//
		// The transition is guarded on the approved state read in STEP 2: a
		// close or reject that slipped in since would otherwise be silently
		// overwritten by 'merged' — and a miss here means the request is no
		// longer the one that was validated, so the whole merge rolls back.
		if err := s.mrs.SetStatus(ctx, tx, mr.ID, repository.MRStatusMerged,
			[]string{repository.MRStatusApproved}, actor, ""); err != nil {
			return err
		}
		if err := tx.Exec(`
			UPDATE merge_requests SET merged_at = now() WHERE id = ?`, mr.ID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`
			UPDATE branches SET status = 'merged', merged_at = now() WHERE id = ?`,
			branch.ID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`
			INSERT INTO audit_events (actor, action, target, metadata)
			VALUES (?, 'merge', ?, ?)`,
			actor, fmt.Sprintf("branch:%s", branch.Name),
			fmt.Sprintf(`{"release_version":%d,"values_applied":%d}`, release.Version, applied),
		).Error; err != nil {
			return err
		}

		result = &Result{
			ReleaseID:      release.ID,
			ReleaseVersion: release.Version,
			ValuesApplied:  applied,
			KeysApplied:    keysApplied,
			BundlesWritten: len(locales),
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	log.Infow(ctx, "merge complete", "branch", branchName,
		"release", result.ReleaseVersion,
		"values", result.ValuesApplied, "keys", result.KeysApplied)
	return result, nil
}

// applyDeltasSQL folds non-removed deltas into master.
//
// `eligible` is every delta this merge intends to land. A delta whose conflict
// was resolved as 'master' is excluded up front: the reviewer chose master's
// value, so the branch's is discarded — but ONLY while that conflict is still
// real. A 'master' resolution recorded against a pair whose versions agree is
// spurious, and honouring it would silently drop a clean delta.
//
// `applicable` is the version guard, re-checked in the statement that writes:
// the delta lands only when master is still at the delta's base_master_version,
// or a human explicitly chose 'mine'. An eligible delta that is NOT applicable
// means master moved after STEP 4 checked the conflicts — that is the blocked
// count, and the caller turns it into ErrConcurrentMasterWrite and rolls back.
//
// One statement, one snapshot: eligibility, the write, the history rows and
// both counts observe the same data, so the counts cannot lie about what was
// applied. The translation_history insert mirrors what SetTranslation records
// for a UI write — the post-change value and version, source 'merge', anchored
// on the branch.
const applyDeltasSQL = `
WITH eligible AS (
    SELECT bt.key_id, bt.locale_id, bt.value, bt.render_hint,
           (COALESCE(t.version, 0) = bt.base_master_version
            OR COALESCE(mcr.resolution, '') = 'mine')       AS applicable
      FROM branch_translations bt
      LEFT JOIN translations t
        ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
      LEFT JOIN merge_conflict_resolutions mcr
        ON mcr.merge_request_id = $2
       AND mcr.key_id = bt.key_id
       AND COALESCE(mcr.locale_id, -1) = bt.locale_id
     WHERE bt.branch_id = $1
       AND NOT bt.is_removed
       AND NOT (COALESCE(mcr.resolution, '') = 'master'
                AND COALESCE(t.version, 0) <> bt.base_master_version)
), applied AS (
    INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
    SELECT e.key_id, e.locale_id, e.value, e.render_hint, 1, $3, now()
      FROM eligible e
     WHERE e.applicable
    ON CONFLICT (key_id, locale_id) DO UPDATE SET
        value       = EXCLUDED.value,
        render_hint = EXCLUDED.render_hint,
        version     = translations.version + 1,
        updated_by  = EXCLUDED.updated_by,
        updated_at  = now()
    RETURNING key_id, locale_id, value, render_hint, version
), recorded AS (
    INSERT INTO translation_history
        (key_id, locale_id, value, render_hint, version, source, branch_id, changed_by)
    SELECT key_id, locale_id, value, render_hint, version, 'merge', $1, $3
      FROM applied
)
SELECT count(*) FILTER (WHERE NOT e.applicable) AS blocked,
       (SELECT count(*) FROM applied)           AS applied
  FROM eligible e`

// removeDeltasSQL applies tombstones — deleting the master row rather than
// blanking it, because removing a translation and setting it to "" are
// different acts and the three-state rule must survive the merge.
//
// Same eligibility and version guard as applyDeltasSQL. The history row
// follows DeleteTranslation's convention: a NULL value says the pair became
// UNTRANSLATED rather than blank, and the version recorded is the one the
// deleted row held — there is no new version to report. A tombstone over a
// pair master never held is applicable but deletes nothing, which is correct
// and leaves no history: nothing changed.
const removeDeltasSQL = `
WITH eligible AS (
    SELECT bt.key_id, bt.locale_id,
           (COALESCE(t.version, 0) = bt.base_master_version
            OR COALESCE(mcr.resolution, '') = 'mine')       AS applicable
      FROM branch_translations bt
      LEFT JOIN translations t
        ON t.key_id = bt.key_id AND t.locale_id = bt.locale_id
      LEFT JOIN merge_conflict_resolutions mcr
        ON mcr.merge_request_id = $2
       AND mcr.key_id = bt.key_id
       AND COALESCE(mcr.locale_id, -1) = bt.locale_id
     WHERE bt.branch_id = $1
       AND bt.is_removed
       AND NOT (COALESCE(mcr.resolution, '') = 'master'
                AND COALESCE(t.version, 0) <> bt.base_master_version)
), removed AS (
    DELETE FROM translations t
     USING eligible e
     WHERE e.applicable
       AND t.key_id = e.key_id AND t.locale_id = e.locale_id
    RETURNING t.key_id, t.locale_id, t.render_hint, t.version
), recorded AS (
    INSERT INTO translation_history
        (key_id, locale_id, value, render_hint, version, source, branch_id, changed_by)
    SELECT key_id, locale_id, NULL, render_hint, version, 'merge', $1, $3
      FROM removed
)
SELECT count(*) FILTER (WHERE NOT e.applicable) AS blocked,
       (SELECT count(*) FROM removed)           AS removed
  FROM eligible e`

func (s *Service) applyDeltas(ctx context.Context, tx *gorm.DB, mrID, branchID int64, actor string) (int, error) {
	var blocked, applied int
	row := tx.Raw(applyDeltasSQL, branchID, mrID, actor).Row()
	if err := row.Scan(&blocked, &applied); err != nil {
		return 0, fmt.Errorf("apply value deltas: %w", err)
	}
	if blocked > 0 {
		return 0, fmt.Errorf("%w: %d value delta(s) no longer match master",
			ErrConcurrentMasterWrite, blocked)
	}

	var removedBlocked, removed int
	row = tx.Raw(removeDeltasSQL, branchID, mrID, actor).Row()
	if err := row.Scan(&removedBlocked, &removed); err != nil {
		return applied, fmt.Errorf("apply removal deltas: %w", err)
	}
	if removedBlocked > 0 {
		return applied, fmt.Errorf("%w: %d removal delta(s) no longer match master",
			ErrConcurrentMasterWrite, removedBlocked)
	}
	return applied + removed, nil
}
