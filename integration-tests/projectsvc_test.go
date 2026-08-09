package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
)

func newProjectSvc(t *testing.T) *projectsvc.Service {
	t.Helper()
	conn := testGORM(t)
	return projectsvc.ProvideService(
		database.ProvideTransactional(conn),
		repository.ProvideProjectRepository(conn),
		repository.ProvideUserProjectRoleRepository(conn),
	)
}

// TestProjectCreateGrantsTheCreatorAdmin proves the one thing projectsvc.Create
// exists to guarantee: a project whose creator holds no role on it is a
// project nobody can configure, so the project row and the creator's admin
// grant land in the SAME transaction.
func TestProjectCreateGrantsTheCreatorAdmin(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	const actor = "projectsvc-creator@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'viewer')`, actor)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM users WHERE email = $1`, actor)
		require.NoError(t, err)
	})

	created, err := svc.Create(ctx, actor, projectsvc.NewProject{
		Code: "projectsvc-it", Name: "Projectsvc Integration Test",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// The role row must go BEFORE the project row: user_project_roles.project_id
		// has no ON DELETE action, so deleting the project first — which is what
		// t.Cleanup's LIFO order would otherwise do, since this is registered
		// after the user cleanup above — would fail its foreign key and (if the
		// error were discarded, as an earlier version of this test did) leave the
		// project behind for the next run to collide with. Deleting the role
		// explicitly here makes this cleanup correct regardless of registration
		// order.
		_, err := testDB.Exec(`DELETE FROM user_project_roles WHERE project_id = $1`, created.ID)
		require.NoError(t, err)
		_, err = testDB.Exec(`DELETE FROM projects WHERE id = $1`, created.ID)
		require.NoError(t, err)
	})

	assert.Equal(t, "active", created.Status)

	var role string
	require.NoError(t, testDB.QueryRow(
		`SELECT role FROM user_project_roles WHERE email = $1 AND project_id = $2`,
		actor, created.ID).Scan(&role))
	assert.Equal(t, repository.RoleAdmin, role, "the creator must be able to administer what they just created")
}

// TestProjectCreateRefusesADuplicateCode proves repository.ErrProjectCodeTaken
// passes straight through the service to the caller, unwrapped further — the
// handler matches it with errors.Is to answer 409, not 500.
//
// This does NOT prove the transaction rolls back: Create fails at
// projects.Create, before roles.Grant is ever reached, so there is nothing to
// roll back on this path. See TestProjectCreateRollsBackTheProjectWhenTheGrantFails
// for the test that actually exercises the rollback direction.
func TestProjectCreateRefusesADuplicateCode(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	const actor = "projectsvc-dup@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'viewer')`, actor)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM users WHERE email = $1`, actor)
		require.NoError(t, err)
	})

	_, err = svc.Create(ctx, actor, projectsvc.NewProject{Code: "youtrip", Name: "Dup"})
	assert.ErrorIs(t, err, repository.ErrProjectCodeTaken)
}

// TestProjectCreateRollsBackTheProjectWhenTheGrantFails is the test that
// actually discriminates on rollback direction.
// TestProjectCreateRefusesADuplicateCode's retired "no stray grant" assertion
// could not fail: Create errors out at projects.Create, before roles.Grant is
// ever reached, so the count it checked was zero whether or not the two
// writes shared a transaction at all.
//
// This test drives the OTHER write order: the project insert succeeds, and
// the grant is what fails, because user_project_roles.email REFERENCES
// users (email) and ghostActor holds no row there. The only way the project
// row can be absent afterward is if projects.Create ran inside the SAME
// transaction as the failing roles.Grant — exactly the promise Create's doc
// comment makes. If either write used a different tx (or nil), the project
// row would survive this error.
func TestProjectCreateRollsBackTheProjectWhenTheGrantFails(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	const code = "projectsvc-rollback-it"
	const ghostActor = "ghost-actor-not-in-users@you.co"

	_, err := svc.Create(ctx, ghostActor, projectsvc.NewProject{Code: code, Name: "Rollback Test"})
	require.Error(t, err)

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM projects WHERE code = $1`, code).Scan(&count))
	assert.Zero(t, count, "the project row must not survive a grant that never committed")
}

// TestProjectCreateValidatesTheCode proves the codePattern check runs before
// anything is written — a malformed code must never reach the database at
// all, let alone as a driver error.
func TestProjectCreateValidatesTheCode(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	cases := []string{"", "UPPER", "1starts-with-digit", "has space", "a"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			_, err := svc.Create(ctx, "someone@you.co", projectsvc.NewProject{Code: code, Name: "X"})
			assert.ErrorIs(t, err, projectsvc.ErrBadRequest)
		})
	}
}

// TestProjectUpdateRejectsAnUnknownStatus.
func TestProjectUpdateRejectsAnUnknownStatus(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	_, err := svc.Update(ctx, "youtrip", projectsvc.ProjectPatch{Name: "YouTrip", Status: "paused"})
	assert.ErrorIs(t, err, projectsvc.ErrBadRequest)
}

// TestProjectUpdateReportsAMissingProject: a typo'd code is a 404, propagated
// as repository.ErrNotFound so the handler need not know a separate sentinel.
func TestProjectUpdateReportsAMissingProject(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	_, err := svc.Update(ctx, "no-such-project", projectsvc.ProjectPatch{Name: "X", Status: "active"})
	assert.ErrorIs(t, err, repository.ErrNotFound)
}
