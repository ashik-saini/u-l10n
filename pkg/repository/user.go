package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
)

// User is a human operator of the portal.
//
// u-l10n owns this table. A row here is NOT derived from the portal's yp_*
// Google Workspace groups: a designer may be an l10n editor and nothing else,
// and overloading another system's authorization model means inheriting its
// every future change. Google answers "who is this?"; this table answers "what
// may they do?".
type User struct {
	ID        int64
	Email     string
	Role      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Roles, in ascending order of privilege. The ordering is the point: middleware
// expresses "editor or above" as one comparison rather than set membership
// repeated at every handler. See route.roleRank.
const (
	RoleViewer   = "viewer"
	RoleEditor   = "editor"
	RoleApprover = "approver"
	RoleAdmin    = "admin"
)

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// UserRepository owns the users table.
//
// Every method takes an optional tx: a role change and its audit row must land
// together or not at all — see base.db for why a repository must never open the
// transaction itself.
//
// No method lowercases an email. The column is CITEXT, so case-insensitivity is
// a property of the type rather than of every call site remembering LOWER().
type UserRepository interface {
	// ByEmail resolves an operator. This is on the path of every authenticated
	// portal request, so it is a single indexed lookup and nothing more.
	//
	// Returns ErrNotFound when the address is unknown. A caller must map that to
	// 403, not 401: the token was valid, the person simply has no account here.
	ByEmail(ctx context.Context, tx *gorm.DB, email string) (User, error)

	// Upsert creates the user or updates their role and status. This is the
	// bootstrap path — see the `user grant` CLI command — and it is idempotent
	// so re-running it is never an error.
	Upsert(ctx context.Context, tx *gorm.DB, u User) (User, error)

	// List returns every user, ordered by email so output is stable.
	List(ctx context.Context, tx *gorm.DB) ([]User, error)

	// SetRole changes an existing user's role. Returns ErrNotFound when there is
	// no such user, so granting a role to a typo'd address is a 404 rather than
	// a silent no-op.
	SetRole(ctx context.Context, tx *gorm.DB, email, role string) (User, error)
}

type userRepository struct{ base }

func ProvideUserRepository(connector database.GORMConnector) UserRepository {
	return &userRepository{base{connector: connector}}
}

const selectUserColumns = `id, email, role, status, created_at, updated_at`

func scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func (r *userRepository) ByEmail(ctx context.Context, tx *gorm.DB, email string) (User, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+selectUserColumns+` FROM users WHERE email = $1`, email).Row()

	u, err := scanUser(row)
	switch {
	case err == nil:
		return u, nil
	case isNoRows(err):
		return u, fmt.Errorf("user %q: %w", email, ErrNotFound)
	default:
		return u, fmt.Errorf("read user %q: %w", email, err)
	}
}

// upsertUserSQL creates or promotes.
//
// Hand-written because GORM v1 has no clause.OnConflict — that is a v2 API and
// it appears nowhere in this codebase.
//
// updated_at is set explicitly: there is no trigger on this table, and a column
// that only ever holds its insert-time default is worse than no column at all.
const upsertUserSQL = `
INSERT INTO users (email, role, status)
VALUES ($1, $2, $3)
ON CONFLICT (email) DO UPDATE
   SET role = EXCLUDED.role, status = EXCLUDED.status, updated_at = now()
RETURNING ` + selectUserColumns

func (r *userRepository) Upsert(ctx context.Context, tx *gorm.DB, u User) (User, error) {
	row := r.db(ctx, tx).Raw(upsertUserSQL, u.Email, u.Role, u.Status).Row()

	created, err := scanUser(row)
	if err != nil {
		return u, fmt.Errorf("upsert user %q: %w", u.Email, err)
	}
	return created, nil
}

func (r *userRepository) List(ctx context.Context, tx *gorm.DB) ([]User, error) {
	rows, err := r.db(ctx, tx).Raw(
		`SELECT ` + selectUserColumns + ` FROM users ORDER BY email`).Rows()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Status,
			&u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

const setRoleSQL = `
UPDATE users SET role = $2, updated_at = now()
 WHERE email = $1
RETURNING ` + selectUserColumns

func (r *userRepository) SetRole(ctx context.Context, tx *gorm.DB, email, role string) (User, error) {
	row := r.db(ctx, tx).Raw(setRoleSQL, email, role).Row()

	u, err := scanUser(row)
	switch {
	case err == nil:
		return u, nil
	case isNoRows(err):
		return u, fmt.Errorf("user %q: %w", email, ErrNotFound)
	default:
		return u, fmt.Errorf("set role for %q: %w", email, err)
	}
}
