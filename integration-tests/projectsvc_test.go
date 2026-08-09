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
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = $1`, actor)
	})

	created, err := svc.Create(ctx, actor, projectsvc.NewProject{
		Code: "projectsvc-it", Name: "Projectsvc Integration Test",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, created.ID)
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
func TestProjectCreateRefusesADuplicateCode(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	const actor = "projectsvc-dup@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'viewer')`, actor)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = $1`, actor)
	})

	_, err = svc.Create(ctx, actor, projectsvc.NewProject{Code: "youtrip", Name: "Dup"})
	assert.ErrorIs(t, err, repository.ErrProjectCodeTaken)

	// The refusal must not have left a stray grant behind for a project that
	// was never created.
	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM user_project_roles WHERE email = $1`, actor).Scan(&count))
	assert.Zero(t, count)
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
