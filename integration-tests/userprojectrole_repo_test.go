package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// TestUserProjectRoleGrantIsIdempotentAndPromotes mirrors
// TestUserUpsertIsIdempotentAndPromotes: `project create` re-running the
// creator's admin grant, or a human re-granting the same person a different
// role, must promote in place rather than failing on the
// (email, project_id) primary key.
func TestUserProjectRoleGrantIsIdempotentAndPromotes(t *testing.T) {
	ctx := context.Background()
	repo := repository.ProvideUserProjectRoleRepository(testGORM(t))

	const email = "grant-repo@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'viewer')`, email)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = $1`, email)
	})

	require.NoError(t, repo.Grant(ctx, nil, email, 1, repository.RoleEditor, "booter@you.co"))

	var role, grantedBy string
	require.NoError(t, testDB.QueryRow(
		`SELECT role, granted_by FROM user_project_roles WHERE email = $1 AND project_id = 1`,
		email).Scan(&role, &grantedBy))
	assert.Equal(t, repository.RoleEditor, role)
	assert.Equal(t, "booter@you.co", grantedBy)

	// Re-granting promotes the existing row rather than colliding with the
	// primary key — the same escape-hatch reasoning as UserRepository.Upsert.
	require.NoError(t, repo.Grant(ctx, nil, email, 1, repository.RoleAdmin, "booter2@you.co"))

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM user_project_roles WHERE email = $1 AND project_id = 1`,
		email).Scan(&count))
	assert.Equal(t, 1, count, "the same grant, not a second row")

	require.NoError(t, testDB.QueryRow(
		`SELECT role, granted_by FROM user_project_roles WHERE email = $1 AND project_id = 1`,
		email).Scan(&role, &grantedBy))
	assert.Equal(t, repository.RoleAdmin, role)
	assert.Equal(t, "booter2@you.co", grantedBy)
}

// TestUserProjectRoleGrantRefusesAnUnknownRole proves the repository does not
// bypass user_project_roles_role_check — a typo must still be rejected by the
// database even though projectsvc validates role vocabulary itself for
// everything except this grant, which always writes "admin" literally.
func TestUserProjectRoleGrantRefusesAnUnknownRole(t *testing.T) {
	ctx := context.Background()
	repo := repository.ProvideUserProjectRoleRepository(testGORM(t))

	const email = "grant-repo-bad-role@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'viewer')`, email)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = $1`, email)
	})

	err = repo.Grant(ctx, nil, email, 1, "superuser", "booter@you.co")
	requireRejected(t, err, "a role outside the ordered set")
}
