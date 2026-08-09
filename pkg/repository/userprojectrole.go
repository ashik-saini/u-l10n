package repository

import (
	"context"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
)

// UserProjectRoleRepository owns user_project_roles: what a person may do, on
// which project — split from identity (users.role) in
// .db/V1.13__scope_identity.sql.
//
// It has exactly one method because that is all any caller needs so far: the
// grant a project's creator receives for it, written in the same transaction
// that creates the project. See projectsvc.Create.
type UserProjectRoleRepository interface {
	// Grant creates or promotes a person's role on a project. Idempotent, for
	// the same reason UserRepository.Upsert is: re-running the grant that
	// bootstraps a project must never fail on a unique violation.
	Grant(ctx context.Context, tx *gorm.DB, email string, projectID int16, role, grantedBy string) error
}

type userProjectRoleRepository struct{ base }

func ProvideUserProjectRoleRepository(connector database.GORMConnector) UserProjectRoleRepository {
	return &userProjectRoleRepository{base{connector: connector}}
}

const grantUserProjectRoleSQL = `
INSERT INTO user_project_roles (email, project_id, role, granted_by)
VALUES (?, ?, ?, ?)
ON CONFLICT (email, project_id) DO UPDATE
   SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by, granted_at = now()`

func (r *userProjectRoleRepository) Grant(
	ctx context.Context, tx *gorm.DB, email string, projectID int16, role, grantedBy string,
) error {
	if err := r.db(ctx, tx).Exec(grantUserProjectRoleSQL, email, projectID, role, grantedBy).Error; err != nil {
		return fmt.Errorf("grant %s role on project %d to %q: %w", role, projectID, email, err)
	}
	return nil
}
