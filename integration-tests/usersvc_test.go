package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/usersvc"
)

func newUserSvc(t *testing.T) *usersvc.Service {
	t.Helper()
	conn := testGORM(t)
	return usersvc.ProvideService(
		database.ProvideTransactional(conn),
		repository.ProvideUserRepository(conn),
		repository.ProvideUserProjectRoleRepository(conn),
		repository.ProvideAuditRepository(conn),
	)
}

// readRole returns users.role and the project-1 user_project_roles.role for
// one person, so a test can compare the two directly. found is false when the
// grant row is absent, which is a different fact from a grant that says
// something stale — and the difference is exactly what these tests are about.
func readRole(t *testing.T, email string) (userRole, grantRole string, found bool) {
	t.Helper()

	require.NoError(t, testDB.QueryRow(
		`SELECT role FROM users WHERE email = $1`, email).Scan(&userRole))

	err := testDB.QueryRow(
		`SELECT role FROM user_project_roles WHERE email = $1 AND project_id = 1`,
		email).Scan(&grantRole)
	if err != nil {
		return userRole, "", false
	}
	return userRole, grantRole, true
}

// TestUserGrantWritesBothRoleTables is the test for the divergence V1.13 opened.
//
// users.role and user_project_roles hold the same fact for as long as the
// transition lasts: the middleware reads the first today and the second after
// Plan 2. Before this fix `user grant` wrote only the first, so a user
// provisioned between the two plans had NO grant row at all — a lockout at
// cutover — and a demotion left the old grant standing, which is the worse
// direction: the demoted admin silently regains admin.
//
// Neither failure raises an error at any step, which is why the assertion has
// to compare the two tables rather than check that the call succeeded.
func TestUserGrantWritesBothRoleTables(t *testing.T) {
	ctx := context.Background()
	svc := newUserSvc(t)

	const email = "usersvc-grant@you.co"
	t.Cleanup(func() {
		// The grant first: user_project_roles_email_fkey cascades, so this is
		// belt-and-braces, but user_project_roles_project_id_fkey does not, and
		// a leaked row here would make every later run fail on the primary key
		// instead of on whatever it was actually testing.
		_, err := testDB.Exec(`DELETE FROM user_project_roles WHERE email = $1`, email)
		require.NoError(t, err)
		_, err = testDB.Exec(`DELETE FROM users WHERE email = $1`, email)
		require.NoError(t, err)
	})

	// Provisioning a brand-new user must produce a grant, not just a users row.
	_, err := svc.Grant(ctx, email, repository.RoleAdmin, repository.StatusActive,
		false, "booter@you.co", "req-grant-1")
	require.NoError(t, err)

	userRole, grantRole, found := readRole(t, email)
	assert.Equal(t, repository.RoleAdmin, userRole)
	require.True(t, found,
		"a user provisioned after V1.13 and before Plan 2 must not be left without a grant")
	assert.Equal(t, repository.RoleAdmin, grantRole)

	// The demotion. This is the direction that escalates rather than locks out:
	// a stale admin grant outranks the viewer users.role the moment the
	// middleware switches tables.
	_, err = svc.Grant(ctx, email, repository.RoleViewer, repository.StatusActive,
		false, "booter@you.co", "req-grant-2")
	require.NoError(t, err)

	userRole, grantRole, found = readRole(t, email)
	assert.Equal(t, repository.RoleViewer, userRole)
	require.True(t, found)
	assert.Equal(t, repository.RoleViewer, grantRole,
		"the grant must follow the demotion, or the user silently regains admin at cutover")

	// One person, one grant per project: re-granting promotes in place.
	var grants int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM user_project_roles WHERE email = $1`, email).Scan(&grants))
	assert.Equal(t, 1, grants)
}

// TestUserSetRoleWritesBothRoleTables covers the API half of the same
// divergence: PATCH /admin/users/{email}/role is the path an operator actually
// uses day to day, and it had the identical gap.
func TestUserSetRoleWritesBothRoleTables(t *testing.T) {
	ctx := context.Background()
	svc := newUserSvc(t)

	const email = "usersvc-setrole@you.co"
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM user_project_roles WHERE email = $1`, email)
		require.NoError(t, err)
		_, err = testDB.Exec(`DELETE FROM users WHERE email = $1`, email)
		require.NoError(t, err)
	})

	// SetRole deliberately does not create the user, so seed one the way V1.13
	// left every pre-existing operator: a users row and a matching grant.
	_, err := testDB.Exec(
		`INSERT INTO users (email, role) VALUES ($1, 'admin')`, email)
	require.NoError(t, err)
	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ($1, 1, 'admin', 'migration:V1.13')`, email)
	require.NoError(t, err)

	_, err = svc.SetRole(ctx, email, repository.RoleEditor, "admin@you.co", "req-setrole-1")
	require.NoError(t, err)

	userRole, grantRole, found := readRole(t, email)
	assert.Equal(t, repository.RoleEditor, userRole)
	require.True(t, found)
	assert.Equal(t, repository.RoleEditor, grantRole,
		"the grant must move with users.role, not keep the role it was migrated with")

	// granted_by records who made the change, so the grant answers the same
	// question the audit row does rather than still naming the migration.
	var grantedBy string
	require.NoError(t, testDB.QueryRow(
		`SELECT granted_by FROM user_project_roles WHERE email = $1 AND project_id = 1`,
		email).Scan(&grantedBy))
	assert.Equal(t, "admin@you.co", grantedBy)
}
