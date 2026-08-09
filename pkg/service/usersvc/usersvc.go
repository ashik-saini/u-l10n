// Package usersvc owns the portal's user records: who exists, what role they
// hold, and the audit trail of how that changed.
//
// It is deliberately tiny. Authentication happens in pkg/googleauth (who is
// this?) and authorization happens in route.RequireIdentity (may they do this?);
// what is left here is the writing of privilege changes, which must be
// transactional with their audit rows and must be reachable from both the API
// and the shell.
//
// Both entry points matter. An empty users table has no admin, so no request
// can ever grant the first role — the API alone cannot bootstrap itself. Grant
// is that escape hatch, and it is the same code path as the API's SetRole so
// the two cannot drift into disagreeing about what a valid role is.
//
// Both also write TWICE: users.role, which the middleware reads today, and the
// matching user_project_roles row, which it will read after Plan 2. Two tables
// hold the same fact for as long as V1.13's transition lasts, and a write path
// that updated only one of them would leave a demoted admin still holding
// admin — see Grant for the full failure.
package usersvc

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

// ErrBadRequest marks a caller error, so the handler maps it to 400 rather than
// 500. An unknown role is a typo, not a fault.
var ErrBadRequest = errors.New("bad request")

// bootstrapProjectID is the project every role written here lands on.
//
// TODO(plan-2): both write paths below still take a role but no project, so
// the grant they mirror into user_project_roles has to name one, and YouTrip
// is the only project any existing operator has. It becomes a parameter when
// the CLI and PATCH /admin/users/{email}/role learn to say which project they
// mean.
const bootstrapProjectID int16 = 1

// Service performs user writes. It owns the transaction boundary; no repository
// it calls opens one.
type Service struct {
	tx    database.Transactional
	users repository.UserRepository
	roles repository.UserProjectRoleRepository
	audit repository.AuditRepository
}

func ProvideService(
	tx database.Transactional,
	users repository.UserRepository,
	roles repository.UserProjectRoleRepository,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, users: users, roles: roles, audit: audit}
}

// SetRole changes an existing user's role.
//
// It does NOT create the user. Creation is the bootstrap path and belongs to
// Grant: an admin who fat-fingers an address should get a 404, not a viewer
// account for a person who does not exist.
func (s *Service) SetRole(
	ctx context.Context, email, role, actor, requestID string,
) (repository.User, error) {
	var updated repository.User

	email, err := normaliseEmail(email)
	if err != nil {
		return updated, err
	}
	if err := validateRole(role); err != nil {
		return updated, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		// Read the previous role inside the transaction that changes it, so the
		// audit row records what actually preceded this write rather than
		// whatever was true a moment earlier.
		before, err := s.users.ByEmail(ctx, tx, email)
		if err != nil {
			return err
		}

		u, err := s.users.SetRole(ctx, tx, email, role)
		if err != nil {
			return err
		}
		updated = u

		// The grant moves with users.role, in the SAME transaction, or the two
		// diverge silently — see Grant below for why that divergence is a
		// privilege escalation rather than an inconsistency.
		if err := s.roles.Grant(ctx, tx, email, bootstrapProjectID, role, actor); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionUserRoleChange,
			Target: "user:" + email,
			Metadata: map[string]any{
				"from": before.Role,
				"to":   u.Role,
			},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.User{}, err
	}

	log.Infow(ctx, "user role changed",
		"email", updated.Email, "role", updated.Role, "actor", actor)
	return updated, nil
}

// Grant creates the user or updates their role and status.
//
// This is what the `user grant` CLI command calls, and it exists because of a
// chicken-and-egg problem the API cannot solve: with an empty users table
// nobody holds the admin role, so no authenticated request is permitted to
// create the first one. Someone with shell and database access has to start the
// chain, and that person is already more privileged than any role this table
// can express.
func (s *Service) Grant(
	ctx context.Context, email, role, status string, isPlatformAdmin bool, actor, requestID string,
) (repository.User, error) {
	var granted repository.User

	email, err := normaliseEmail(email)
	if err != nil {
		return granted, err
	}
	if err := validateRole(role); err != nil {
		return granted, err
	}
	if status == "" {
		status = repository.StatusActive
	}
	if status != repository.StatusActive && status != repository.StatusDisabled {
		return granted, fmt.Errorf("%w: status must be %s or %s, got %q",
			ErrBadRequest, repository.StatusActive, repository.StatusDisabled, status)
	}
	if strings.TrimSpace(actor) == "" {
		// audit_events.actor is NOT NULL and a blank actor answers nothing. The
		// CLI has no authenticated principal, so the human running it must name
		// themselves.
		return granted, fmt.Errorf("%w: actor is required", ErrBadRequest)
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		// Absent and present are different facts and the audit row should say
		// which. ErrNotFound here is the normal case, not a failure.
		previous := "(none)"
		switch before, err := s.users.ByEmail(ctx, tx, email); {
		case err == nil:
			previous = before.Role
		case errors.Is(err, repository.ErrNotFound):
		default:
			return err
		}

		u, err := s.users.Upsert(ctx, tx, repository.User{
			Email:           email,
			Role:            role,
			Status:          status,
			IsPlatformAdmin: isPlatformAdmin,
		})
		if err != nil {
			return err
		}
		granted = u

		// V1.13 backfilled a project-1 grant for every user that existed then,
		// and projectsvc.Create writes one for each new project's creator.
		// Nothing else did — so between that migration and Plan 2, every user
		// this command creates would have a users.role and NO grant, and every
		// demotion would leave a stale grant behind. Both are silent today
		// (the middleware still reads users.role) and both surface at the
		// cutover: the first as a lockout, the second as a demoted admin
		// quietly regaining admin. Writing both here, in one transaction, is
		// what stops the two tables drifting while both exist.
		if err := s.roles.Grant(ctx, tx, email, bootstrapProjectID, role, actor); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionUserGrant,
			Target: "user:" + email,
			Metadata: map[string]any{
				"from":           previous,
				"to":             u.Role,
				"status":         u.Status,
				"platform_admin": u.IsPlatformAdmin,
				"via":            "cli",
			},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.User{}, err
	}

	log.Infow(ctx, "user granted",
		"email", granted.Email, "role", granted.Role, "status", granted.Status,
		"platform_admin", granted.IsPlatformAdmin, "actor", actor)
	return granted, nil
}

// List returns every user. Read-only, so no transaction.
func (s *Service) List(ctx context.Context) ([]repository.User, error) {
	return s.users.List(ctx, nil)
}

// validateRole rejects anything outside the ordered set.
//
// The users_role_check constraint would reject it too, but a constraint
// violation surfaces as a 500 with a Postgres error string in it. This is the
// caller's mistake and must read as one.
func validateRole(role string) error {
	switch role {
	case repository.RoleViewer, repository.RoleEditor,
		repository.RoleApprover, repository.RoleAdmin:
		return nil
	default:
		return fmt.Errorf("%w: role must be one of %s, %s, %s, %s — got %q",
			ErrBadRequest, repository.RoleViewer, repository.RoleEditor,
			repository.RoleApprover, repository.RoleAdmin, role)
	}
}

// normaliseEmail trims and sanity-checks an address.
//
// It does NOT lowercase: users.email is CITEXT, so case is handled by the
// column type. Lowercasing here would work but would put a second, silently
// divergent normalisation rule in the codebase.
func normaliseEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", fmt.Errorf("%w: email is required", ErrBadRequest)
	}
	// The cheapest check that rejects a path segment mistaken for an address.
	// Real address validation belongs to whoever issues the Google token.
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.ContainsAny(email, " \t\r\n") {
		return "", fmt.Errorf("%w: %q is not an email address", ErrBadRequest, email)
	}
	return email, nil
}
