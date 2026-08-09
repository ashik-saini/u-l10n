// Package branchsvc owns the lifecycle of a branch: creating one, closing it,
// reopening it, and showing what it has changed.
//
// A branch is a named workspace holding copy-on-write deltas over master. It
// does NOT copy master's ~36,000 values, so creating one is a single INSERT and
// costs nothing — which is the point. The expensive part of branching is
// reconciling it, and that lives in mergesvc.
//
// Writing to a branch is what pkg/service/keysvc does; this package never
// touches a delta. The split is deliberate: an editor changing copy and an
// editor managing workspaces are different actions, and folding them together
// would put branch lifecycle rules on the hot path of every cell edit.
package branchsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

const (
	// maxBranchNameLen bounds a branch name. Nothing in the schema caps it, but
	// the name is a URL path segment on every branch endpoint.
	maxBranchNameLen = 100

	maxDescriptionLen = 2000
)

var (
	// ErrBadRequest marks a caller error so the handler maps it to 400.
	ErrBadRequest = errors.New("bad request")

	// ErrBranchNotOpen means the branch is merged or closed, and the requested
	// transition is not available from there.
	ErrBranchNotOpen = errors.New("branch is not open")
)

// Service manages branches. It owns every transaction boundary below.
type Service struct {
	tx       database.Transactional
	branches repository.BranchRepository
	audit    repository.AuditRepository
}

func ProvideService(
	tx database.Transactional,
	branches repository.BranchRepository,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, branches: branches, audit: audit}
}

// List returns every branch with its delta counts and live merge request.
//
// status empty means all of them. Read-only, so no transaction: one query
// answers the whole page.
func (s *Service) List(ctx context.Context, status string) ([]repository.BranchSummary, error) {
	if err := validateStatusFilter(status); err != nil {
		return nil, err
	}
	return s.branches.Summaries(ctx, nil, status)
}

// Get returns one branch with its counts.
//
// It goes through SummaryByID rather than ByName alone so the detail view and
// the list cannot disagree about how many changes a branch carries — the
// counts come from the same statement Summaries uses, scoped to one row.
func (s *Service) Get(ctx context.Context, name string) (repository.BranchSummary, error) {
	var out repository.BranchSummary

	branch, err := s.branches.ByName(ctx, nil, name)
	if err != nil {
		return out, err
	}
	// A miss here is only reachable if the branch was deleted between the two
	// reads; SummaryByID reports it as absent, which is the honest answer.
	return s.branches.SummaryByID(ctx, nil, branch.ID)
}

// Create opens a new branch.
func (s *Service) Create(
	ctx context.Context, name, description, actor, requestID string,
) (repository.Branch, error) {
	var created repository.Branch

	name, err := validateName(name)
	if err != nil {
		return created, err
	}
	if len(description) > maxDescriptionLen {
		return created, fmt.Errorf("%w: description must be at most %d characters",
			ErrBadRequest, maxDescriptionLen)
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		b, err := s.branches.Create(ctx, tx, name, strings.TrimSpace(description), actor)
		if err != nil {
			return err
		}
		created = b

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionBranchCreate,
			Target:    "branch:" + b.Name,
			Metadata:  map[string]any{"description": b.Description},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.Branch{}, err
	}

	log.Infow(ctx, "branch created", "branch", created.Name, "actor", actor)
	return created, nil
}

// Close abandons a branch without merging it.
//
// The deltas survive. A closed branch is a decision not to ship those changes,
// and the record of what was proposed is worth keeping — deleting it would make
// "why did we not do this?" unanswerable.
func (s *Service) Close(ctx context.Context, name, actor, requestID string) (repository.Branch, error) {
	return s.transition(ctx, name, repository.BranchStatusClosed,
		repository.ActionBranchClose, actor, requestID)
}

// Reopen returns a closed branch to editable.
//
// A MERGED branch cannot be reopened: its deltas are the permanent record of
// what a merge actually applied, and editing them would edit history. The
// repository enforces that in the UPDATE's own predicate, so no caller can
// forget; this checks first only so the refusal reads as a sentence rather than
// as a missing row.
func (s *Service) Reopen(ctx context.Context, name, actor, requestID string) (repository.Branch, error) {
	return s.transition(ctx, name, repository.BranchStatusOpen,
		repository.ActionBranchReopen, actor, requestID)
}

func (s *Service) transition(
	ctx context.Context, name, status, action, actor, requestID string,
) (repository.Branch, error) {
	var moved repository.Branch

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.branches.ByName(ctx, tx, name)
		if err != nil {
			return err
		}
		if branch.Status == repository.BranchStatusMerged {
			return fmt.Errorf("%w: branch %q has been merged and cannot be reopened or closed",
				ErrBranchNotOpen, name)
		}
		if branch.Status == status {
			// Already there. Not an error — a retried request after a dropped
			// response must not fail — but there is nothing to record either.
			moved = branch
			return nil
		}

		if err := s.branches.SetStatus(ctx, tx, branch.ID, status); err != nil {
			return err
		}
		branch.Status = status
		moved = branch

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    action,
			Target:    "branch:" + name,
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.Branch{}, err
	}

	log.Infow(ctx, "branch status changed", "branch", name, "status", status, "actor", actor)
	return moved, nil
}

// Changes returns the branch's complete diff against master.
//
// Each row is flagged with whether master has moved underneath it, using the
// SAME version comparison the merge performs. A diff that disagreed with the
// merge about what conflicts would be a diff nobody could act on: a reviewer
// would approve a clean-looking change and watch the merge refuse it.
//
// The flags are advisory in the way every pre-transaction check is: they are
// computed outside the merge's advisory lock, so master can move between this
// call and the merge. That is the correct trade — this is a view, and the merge
// re-computes everything inside its own transaction before it applies anything.
func (s *Service) Changes(ctx context.Context, name string) (repository.Branch, repository.BranchChanges, error) {
	var changes repository.BranchChanges

	branch, err := s.branches.ByName(ctx, nil, name)
	if err != nil {
		return branch, changes, err
	}

	changes, err = s.branches.Changes(ctx, nil, branch.ID)
	return branch, changes, err
}

// validateName enforces what the schema does not.
//
// The name is a URL path segment on every branch endpoint, and chi's {name}
// parameter does not match a slash — so a branch called "release/4.12" would be
// creatable and then unreachable. Refusing at creation is the only point where
// that is fixable.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: name is required", ErrBadRequest)
	}
	if len(name) > maxBranchNameLen {
		return "", fmt.Errorf("%w: name must be at most %d characters, got %d",
			ErrBadRequest, maxBranchNameLen, len(name))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return "", fmt.Errorf(
				"%w: name may contain only letters, digits, '-', '_' and '.' — %q is not addressable in a URL path",
				ErrBadRequest, name)
		}
	}
	return name, nil
}

func validateStatusFilter(status string) error {
	switch status {
	case "", repository.BranchStatusOpen,
		repository.BranchStatusMerged, repository.BranchStatusClosed:
		return nil
	default:
		// Silently returning everything for an unrecognised filter would show a
		// portal a list it did not ask for and cannot explain.
		return fmt.Errorf("%w: status must be open, merged or closed, got %q",
			ErrBadRequest, status)
	}
}
