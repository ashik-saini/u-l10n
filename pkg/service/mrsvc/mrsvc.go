// Package mrsvc owns the review workflow around a merge request: opening one,
// moving it through review, recording conflict decisions, and handing the final
// act to pkg/service/mergesvc.
//
// It performs no merge itself. The merge is one transaction under an advisory
// lock and it re-validates everything from scratch; anything this package
// checked beforehand was stale the moment it returned. What lives here is the
// workflow — which transitions are legal, who may make them, and what a
// reviewer needs to see before they decide.
package mrsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
)

var log = ulog.GetLogger("u-l10n")

const (
	maxTitleLen   = 200
	maxCommentLen = 4000
)

var (
	// ErrBadRequest marks a caller error so the handler maps it to 400.
	ErrBadRequest = errors.New("bad request")

	// ErrNotLive means the request is not in a state this transition can be
	// made from — approving a merged request, reopening an open one.
	//
	// A 409, never a 404: the request is right there, it is its STATE that
	// refuses. A 404 would send a reviewer looking for a request they can see
	// on their screen.
	ErrNotLive = errors.New("merge request is not in a state that allows this")
)

// Review actions, as the API names them.
const (
	ActionApprove        = "approve"
	ActionRequestChanges = "request-changes"
	ActionReject         = "reject"
	ActionReopen         = "reopen"
	ActionClose          = "close"
)

// transitions is the whole state machine, in one table.
//
// Written out rather than expressed as conditionals scattered through five
// methods, because "which states may I approve from?" is a question a reviewer
// asks and an auditor asks, and it should have exactly one answer to read.
//
//	open              -> approved | changes_requested | rejected | closed
//	changes_requested -> approved | rejected | closed
//	approved          -> changes_requested | rejected | closed
//	rejected | closed -> open   (subject to one live request per branch)
//	merged            -> nothing. It is done.
var transitions = map[string]struct {
	to   string
	from []string
}{
	ActionApprove: {repository.MRStatusApproved,
		[]string{repository.MRStatusOpen, repository.MRStatusChangesRequested}},
	ActionRequestChanges: {repository.MRStatusChangesRequested,
		[]string{repository.MRStatusOpen, repository.MRStatusApproved}},
	ActionReject: {repository.MRStatusRejected,
		[]string{repository.MRStatusOpen, repository.MRStatusApproved, repository.MRStatusChangesRequested}},
	ActionClose: {repository.MRStatusClosed,
		[]string{repository.MRStatusOpen, repository.MRStatusApproved, repository.MRStatusChangesRequested}},
	ActionReopen: {repository.MRStatusOpen,
		[]string{repository.MRStatusRejected, repository.MRStatusClosed}},
}

// Conflicts is everything blocking a merge, in all THREE kinds.
//
// They are genuinely different and a portal must render them differently:
//
//   - Values: two people edited the same string. Resolvable by choosing a side.
//   - Metadata: two people changed the same key's name or status. Also
//     resolvable by choosing a side, but anchored on keys.version rather than
//     translations.version.
//   - Name collisions: both sides independently claimed a name. NOT resolvable
//     by choosing a side — one of them has to be renamed — which is why they
//     carry no resolution field.
type Conflicts struct {
	MergeRequest repository.MergeRequest
	BranchName   string

	Values     []repository.Conflict
	Meta       []repository.MetaConflict
	Collisions []repository.NameCollision

	// Unresolved counts the value and metadata conflicts still awaiting a
	// decision. Collisions are not counted because no decision would clear them.
	Unresolved int
}

// Mergeable reports whether the merge would get past its conflict checks.
//
// Advisory only. The merge re-computes all of this inside its own transaction,
// under the advisory lock, and that is the answer that counts — this one can be
// stale before the reviewer's finger leaves the button.
func (c Conflicts) Mergeable() bool {
	return c.Unresolved == 0 && len(c.Collisions) == 0
}

// Detail is a merge request with everything the review screen shows.
type Detail struct {
	MergeRequest repository.MergeRequest
	Branch       repository.BranchSummary
	Events       []repository.MergeRequestEvent
	Conflicts    Conflicts
}

// valueConflictKey identifies one (key, locale) pair in the conflict set.
type valueConflictKey struct {
	KeyID    int64
	LocaleID int16
}

// Resolution is one human decision about one conflict.
type Resolution struct {
	KeyID int64
	// LocaleCode empty means this decides a key-METADATA conflict, which has no
	// locale dimension.
	LocaleCode string
	// Choice is "mine" (the branch) or "master".
	Choice string
}

// Service runs the review workflow. It owns every transaction boundary below.
type Service struct {
	tx       database.Transactional
	branches repository.BranchRepository
	mrs      repository.MergeRequestRepository
	locales  repository.LocaleRepository
	merge    *mergesvc.Service
	audit    repository.AuditRepository
}

func ProvideService(
	tx database.Transactional,
	branches repository.BranchRepository,
	mrs repository.MergeRequestRepository,
	locales repository.LocaleRepository,
	merge *mergesvc.Service,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, branches: branches, mrs: mrs,
		locales: locales, merge: merge, audit: audit}
}

// Create opens a review on a branch.
func (s *Service) Create(
	ctx context.Context, branchName, title, actor, requestID string,
) (repository.MergeRequest, error) {
	var created repository.MergeRequest

	title = strings.TrimSpace(title)
	if title == "" {
		return created, fmt.Errorf("%w: title is required", ErrBadRequest)
	}
	if len(title) > maxTitleLen {
		return created, fmt.Errorf("%w: title must be at most %d characters, got %d",
			ErrBadRequest, maxTitleLen, len(title))
	}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.branches.ByName(ctx, tx, branchName)
		if err != nil {
			return err
		}
		if branch.Status != repository.BranchStatusOpen {
			// Reviewing a merged or closed branch would be reviewing a decision
			// already taken.
			return fmt.Errorf("%w: branch %q is %s", ErrNotLive, branch.Name, branch.Status)
		}

		mr, err := s.mrs.Create(ctx, tx, branch.ID, title, actor)
		if err != nil {
			return err
		}
		created = mr

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionMRCreate,
			Target:    fmt.Sprintf("merge_request:%d", mr.ID),
			Metadata:  map[string]any{"branch": branch.Name, "title": title},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.MergeRequest{}, err
	}

	log.Infow(ctx, "merge request created",
		"merge_request", created.ID, "branch", branchName, "actor", actor)
	return created, nil
}

// List returns requests, newest first. An empty status means every state.
func (s *Service) List(ctx context.Context, status string) ([]repository.MergeRequestListing, error) {
	if err := validateStatusFilter(status); err != nil {
		return nil, err
	}
	return s.mrs.List(ctx, nil, status)
}

// Get returns one request with its branch, its timeline and its conflicts.
//
// The conflicts come along because a review screen that had to fetch them
// separately would render "ready to merge" for the moment before the second
// request answered, and that moment is when somebody clicks.
func (s *Service) Get(ctx context.Context, mrID int64) (Detail, error) {
	var detail Detail

	mr, err := s.mrs.ByID(ctx, nil, mrID)
	if err != nil {
		return detail, err
	}
	detail.MergeRequest = mr

	detail.Branch, err = s.branches.SummaryByID(ctx, nil, mr.BranchID)
	if err != nil {
		return detail, err
	}

	detail.Events, err = s.mrs.Events(ctx, nil, mrID)
	if err != nil {
		return detail, err
	}

	detail.Conflicts, err = s.conflicts(ctx, nil, mr, detail.Branch.Name)
	if err != nil {
		return detail, err
	}
	return detail, nil
}

// Conflicts computes everything blocking the merge.
func (s *Service) Conflicts(ctx context.Context, mrID int64) (Conflicts, error) {
	var out Conflicts

	mr, err := s.mrs.ByID(ctx, nil, mrID)
	if err != nil {
		return out, err
	}
	branch, err := s.branchByID(ctx, nil, mr.BranchID)
	if err != nil {
		return out, err
	}
	return s.conflicts(ctx, nil, mr, branch.Name)
}

func (s *Service) conflicts(
	ctx context.Context, tx *gorm.DB, mr repository.MergeRequest, branchName string,
) (Conflicts, error) {
	out := Conflicts{MergeRequest: mr, BranchName: branchName}

	values, err := s.mrs.Conflicts(ctx, tx, mr.ID, mr.BranchID)
	if err != nil {
		return out, err
	}
	meta, err := s.mrs.MetaConflicts(ctx, tx, mr.ID, mr.BranchID)
	if err != nil {
		return out, err
	}
	collisions, err := s.mrs.NameCollisions(ctx, tx, mr.BranchID)
	if err != nil {
		return out, err
	}

	out.Values, out.Meta, out.Collisions = values, meta, collisions
	for _, c := range values {
		if !c.Resolved() {
			out.Unresolved++
		}
	}
	for _, c := range meta {
		if !c.Resolved() {
			out.Unresolved++
		}
	}
	return out, nil
}

// Review moves the request through the workflow.
//
// One method for all five transitions, driven by the table above, so a state
// machine cannot end up with five subtly different implementations of "is this
// legal from here?".
func (s *Service) Review(
	ctx context.Context, mrID int64, action, comment, actor, requestID string,
) (repository.MergeRequest, error) {
	var moved repository.MergeRequest

	rule, ok := transitions[action]
	if !ok {
		return moved, fmt.Errorf("%w: unknown action %q, want %s, %s, %s, %s or %s",
			ErrBadRequest, action, ActionApprove, ActionRequestChanges,
			ActionReject, ActionReopen, ActionClose)
	}
	comment = strings.TrimSpace(comment)
	if len(comment) > maxCommentLen {
		return moved, fmt.Errorf("%w: comment must be at most %d characters",
			ErrBadRequest, maxCommentLen)
	}
	if action == ActionRequestChanges && comment == "" {
		// "Changes requested" with no reason is a reviewer sending work back
		// without saying what is wrong. The other transitions do not need one.
		return moved, fmt.Errorf("%w: request-changes requires a comment saying what to change",
			ErrBadRequest)
	}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		mr, err := s.mrs.ByID(ctx, tx, mrID)
		if err != nil {
			return err
		}
		if !allows(rule.from, mr.Status) {
			return fmt.Errorf("%w: cannot %s a %s merge request (allowed from: %s)",
				ErrNotLive, action, mr.Status, strings.Join(rule.from, ", "))
		}

		// Approve is its own repository call because it also records who and
		// when — approved_at is what the merge compares against the branch's
		// last_edited_at, and a plain status change would leave it NULL and the
		// staleness check inert.
		if action == ActionApprove {
			if err := s.mrs.Approve(ctx, tx, mrID, actor); err != nil {
				return err
			}
		} else if err := s.mrs.SetStatus(ctx, tx, mrID, rule.to, rule.from, actor, comment); err != nil {
			// The check above read the status without a lock, so a concurrent
			// transition — a merge committing while this close is in flight —
			// can still slip between it and the UPDATE. The state predicate
			// inside SetStatus is the guard that actually holds; surface its
			// miss as the same 409 the pre-check produces.
			if errors.Is(err, repository.ErrStaleMergeRequestStatus) {
				return fmt.Errorf("%w: cannot %s this merge request, its status just changed (allowed from: %s)",
					ErrNotLive, action, strings.Join(rule.from, ", "))
			}
			return err
		}

		mr.Status = rule.to
		moved = mr

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionMRReview,
			Target:    fmt.Sprintf("merge_request:%d", mrID),
			Metadata:  map[string]any{"action": action, "to": rule.to, "comment": comment},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.MergeRequest{}, err
	}

	log.Infow(ctx, "merge request reviewed",
		"merge_request", mrID, "action", action, "actor", actor)
	return moved, nil
}

// Resolve records human decisions about conflicts.
//
// All of them in ONE transaction: a partially recorded resolution set would
// leave a reviewer believing they had decided everything while the merge still
// refuses, with no indication of which decision was lost.
func (s *Service) Resolve(
	ctx context.Context, mrID int64, resolutions []Resolution, actor, requestID string,
) (Conflicts, error) {
	var out Conflicts

	if len(resolutions) == 0 {
		return out, fmt.Errorf("%w: at least one resolution is required", ErrBadRequest)
	}
	for _, r := range resolutions {
		if r.KeyID <= 0 {
			return out, fmt.Errorf("%w: key_id is required on every resolution", ErrBadRequest)
		}
		switch r.Choice {
		case "mine", "master":
		default:
			return out, fmt.Errorf("%w: resolution must be mine or master, got %q",
				ErrBadRequest, r.Choice)
		}
	}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		mr, err := s.mrs.ByID(ctx, tx, mrID)
		if err != nil {
			return err
		}
		if isTerminal(mr.Status) {
			return fmt.Errorf("%w: merge request %d is %s", ErrNotLive, mrID, mr.Status)
		}

		// Every resolution must decide a conflict that actually EXISTS. The
		// merge honours a 'master' resolution by discarding the branch's side,
		// so a resolution recorded against a clean pair would silently throw
		// away a delta nobody disputed. Computed inside this transaction so
		// the set checked is the set the rows land against.
		values, err := s.mrs.Conflicts(ctx, tx, mrID, mr.BranchID)
		if err != nil {
			return err
		}
		valueConflicts := make(map[valueConflictKey]bool, len(values))
		for _, c := range values {
			valueConflicts[valueConflictKey{c.KeyID, c.LocaleID}] = true
		}
		meta, err := s.mrs.MetaConflicts(ctx, tx, mrID, mr.BranchID)
		if err != nil {
			return err
		}
		metaConflicts := make(map[int64]bool, len(meta))
		for _, c := range meta {
			metaConflicts[c.KeyID] = true
		}

		for _, r := range resolutions {
			if r.LocaleCode == "" {
				// No locale means a key-metadata conflict, stored with a NULL
				// locale_id. Routing it through Resolve instead would record it
				// against a locale that does not exist.
				if !metaConflicts[r.KeyID] {
					return fmt.Errorf("%w: key %d has no metadata conflict to resolve",
						ErrBadRequest, r.KeyID)
				}
				if err := s.mrs.ResolveMeta(ctx, tx, mrID, r.KeyID, r.Choice, actor); err != nil {
					return err
				}
				continue
			}

			locale, err := s.locales.ByCode(ctx, tx, r.LocaleCode)
			if err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					return fmt.Errorf("%w: unknown locale %q", ErrBadRequest, r.LocaleCode)
				}
				return err
			}
			if !valueConflicts[valueConflictKey{r.KeyID, locale.ID}] {
				return fmt.Errorf("%w: (key %d, locale %s) has no conflict to resolve",
					ErrBadRequest, r.KeyID, r.LocaleCode)
			}
			if err := s.mrs.Resolve(ctx, tx, mrID, r.KeyID, locale.ID, r.Choice, actor); err != nil {
				return err
			}
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionMRResolve,
			Target:    fmt.Sprintf("merge_request:%d", mrID),
			Metadata:  map[string]any{"count": len(resolutions)},
			RequestID: requestID,
		})
	})
	if err != nil {
		return out, err
	}

	log.Infow(ctx, "conflict resolutions recorded",
		"merge_request", mrID, "count", len(resolutions), "actor", actor)

	// Return the recomputed conflict set, so the reviewer sees what is left
	// without a second request that could observe a different world.
	return s.Conflicts(ctx, mrID)
}

// Merge folds the branch into master and cuts a release.
//
// It resolves the branch name and hands over. Every check that matters — is it
// approved, has the branch moved since, are the conflicts resolved, do the names
// collide — happens INSIDE mergesvc's transaction, under its advisory lock,
// because anything verified out here is stale by the time the merge runs.
func (s *Service) Merge(
	ctx context.Context, mrID int64, actor, requestID string,
) (*mergesvc.Result, error) {
	mr, err := s.mrs.ByID(ctx, nil, mrID)
	if err != nil {
		return nil, err
	}
	branch, err := s.branchByID(ctx, nil, mr.BranchID)
	if err != nil {
		return nil, err
	}

	// A cheap, honest early exit for the states no amount of re-checking would
	// rescue. It is NOT the guard — mergesvc re-reads the status inside its
	// transaction — it just avoids taking a global advisory lock to tell
	// somebody their request was rejected last week.
	if isTerminal(mr.Status) {
		return nil, fmt.Errorf("%w: merge request %d is %s", ErrNotLive, mrID, mr.Status)
	}

	result, err := s.merge.Merge(ctx, branch.Name, actor)
	if err != nil {
		return nil, err
	}

	log.Infow(ctx, "merge request merged", "merge_request", mrID,
		"branch", branch.Name, "release", result.ReleaseVersion, "actor", actor)
	return result, nil
}

// branchByID resolves a branch from a merge request's foreign key.
//
// BranchRepository has no ByID — nothing else needs one, since every branch
// endpoint keys on the name — so this goes through SummaryByID, the same one
// query the detail view uses, scoped to one row rather than fetching every
// branch in the system to find one.
func (s *Service) branchByID(ctx context.Context, tx *gorm.DB, branchID int64) (repository.Branch, error) {
	summary, err := s.branches.SummaryByID(ctx, tx, branchID)
	if err != nil {
		return repository.Branch{}, err
	}
	return summary.Branch, nil
}

func allows(from []string, status string) bool {
	for _, s := range from {
		if s == status {
			return true
		}
	}
	return false
}

// isTerminal reports the states nothing moves out of except an explicit reopen.
func isTerminal(status string) bool {
	switch status {
	case repository.MRStatusMerged, repository.MRStatusRejected, repository.MRStatusClosed:
		return true
	default:
		return false
	}
}

func validateStatusFilter(status string) error {
	switch status {
	case "", repository.MRStatusOpen, repository.MRStatusApproved,
		repository.MRStatusChangesRequested, repository.MRStatusRejected,
		repository.MRStatusMerged, repository.MRStatusClosed:
		return nil
	default:
		return fmt.Errorf("%w: unknown status %q", ErrBadRequest, status)
	}
}
