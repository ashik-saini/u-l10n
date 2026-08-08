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

var (
	// ErrStaleApproval means the branch changed after it was approved, so the
	// diff a reviewer signed off is not the diff being merged.
	ErrStaleApproval = errors.New("approval is stale: the branch was edited after approval")

	// ErrNotApproved means the request never reached an approved state.
	ErrNotApproved = errors.New("merge request is not approved")

	// ErrUnresolvedConflicts carries the conflicts still awaiting a human.
	ErrUnresolvedConflicts = errors.New("unresolved conflicts")
)

// UnresolvedError reports which conflicts blocked a merge.
type UnresolvedError struct {
	Conflicts []repository.Conflict
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("%v: %d conflict(s) require a decision", ErrUnresolvedConflicts, len(e.Conflicts))
}
func (e *UnresolvedError) Unwrap() error { return ErrUnresolvedConflicts }

// Result describes a completed merge.
type Result struct {
	ReleaseID      int64
	ReleaseVersion int64
	ValuesApplied  int
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

		// STEP 4 — every conflict must already have a human decision.
		//
		// Auto-resolving means silently choosing one person's words over
		// another's. Return the list and make someone choose.
		conflicts, err := s.mrs.Conflicts(ctx, tx, mr.ID, branch.ID)
		if err != nil {
			return err
		}
		var unresolved []repository.Conflict
		for _, c := range conflicts {
			if !c.Resolved() {
				unresolved = append(unresolved, c)
			}
		}
		if len(unresolved) > 0 {
			return &UnresolvedError{Conflicts: unresolved}
		}

		// STEP 5 — apply the value deltas.
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
		if err := s.mrs.SetStatus(ctx, tx, mr.ID, repository.MRStatusMerged, actor, ""); err != nil {
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
			BundlesWritten: len(locales),
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	log.Infow(ctx, "merge complete", "branch", branchName,
		"release", result.ReleaseVersion, "values", result.ValuesApplied)
	return result, nil
}

// applyDeltasSQL folds non-removed deltas into master.
//
// Deltas whose conflict was resolved as 'master' are skipped: the reviewer chose
// master's value, so the branch's is discarded. Unconflicted deltas have no
// resolution row and are always applied.
const applyDeltasSQL = `
INSERT INTO translations (key_id, locale_id, value, render_hint, version, updated_by, updated_at)
SELECT bt.key_id, bt.locale_id, bt.value, bt.render_hint, 1, $3, now()
  FROM branch_translations bt
  LEFT JOIN merge_conflict_resolutions mcr
    ON mcr.merge_request_id = $2
   AND mcr.key_id = bt.key_id
   AND COALESCE(mcr.locale_id, -1) = bt.locale_id
 WHERE bt.branch_id = $1
   AND NOT bt.is_removed
   AND COALESCE(mcr.resolution, 'mine') <> 'master'
ON CONFLICT (key_id, locale_id) DO UPDATE SET
    value       = EXCLUDED.value,
    render_hint = EXCLUDED.render_hint,
    version     = translations.version + 1,
    updated_by  = EXCLUDED.updated_by,
    updated_at  = now()`

// removeDeltasSQL applies tombstones — deleting the master row rather than
// blanking it, because removing a translation and setting it to "" are
// different acts and the three-state rule must survive the merge.
const removeDeltasSQL = `
DELETE FROM translations t
 USING branch_translations bt
  LEFT JOIN merge_conflict_resolutions mcr
    ON mcr.merge_request_id = $2
   AND mcr.key_id = bt.key_id
   AND COALESCE(mcr.locale_id, -1) = bt.locale_id
 WHERE bt.branch_id = $1
   AND bt.is_removed
   AND COALESCE(mcr.resolution, 'mine') <> 'master'
   AND t.key_id = bt.key_id
   AND t.locale_id = bt.locale_id`

func (s *Service) applyDeltas(ctx context.Context, tx *gorm.DB, mrID, branchID int64, actor string) (int, error) {
	res := tx.Exec(applyDeltasSQL, branchID, mrID, actor)
	if res.Error != nil {
		return 0, fmt.Errorf("apply value deltas: %w", res.Error)
	}
	applied := int(res.RowsAffected)

	res = tx.Exec(removeDeltasSQL, branchID, mrID)
	if res.Error != nil {
		return applied, fmt.Errorf("apply removal deltas: %w", res.Error)
	}
	return applied + int(res.RowsAffected), nil
}
