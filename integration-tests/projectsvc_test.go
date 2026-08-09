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
		repository.ProvideLocaleRepository(conn),
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

	// The assertion below is that this project row does NOT exist, so there is
	// nothing to clean up when the test passes — which is precisely why the
	// cleanup matters. If the rollback ever regresses, the row survives, and
	// without this every subsequent run would fail on projects_code_unique
	// during Create instead of on the assertion that would have named the real
	// problem. A cleanup registered for the failure case only is still a
	// cleanup.
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(
			`DELETE FROM user_project_roles WHERE project_id IN
			     (SELECT id FROM projects WHERE code = $1)`, code)
		require.NoError(t, cleanupErr)
		_, cleanupErr = testDB.Exec(`DELETE FROM projects WHERE code = $1`, code)
		require.NoError(t, cleanupErr)
	})

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

// TestAddLocaleValidatesBeforeWriting proves every one of AddLocale's
// validation rules runs BEFORE the transaction opens — a malformed request
// must never reach the database at all, let alone as a driver error. Each
// case changes exactly one field away from an otherwise-valid request, so a
// validator that stopped checking its one rule would show up as exactly one
// case going from ErrBadRequest to nil (or to a driver error).
func TestAddLocaleValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	valid := func() projectsvc.NewLocale {
		return projectsvc.NewLocale{
			Code: "km-KH", FlutterDir: "km_KH",
			AndroidValuesDir: "values-km", IOSLproj: "km-KH.lproj", SortOrder: 9,
		}
	}

	cases := []struct {
		name   string
		mutate func(*projectsvc.NewLocale)
	}{
		{"lowercase-only code", func(l *projectsvc.NewLocale) { l.Code = "KM" }},
		{"three-letter code", func(l *projectsvc.NewLocale) { l.Code = "khm" }},
		{"lowercase region", func(l *projectsvc.NewLocale) { l.Code = "km-kh" }},
		{"empty code", func(l *projectsvc.NewLocale) { l.Code = "" }},
		{"empty flutter_dir", func(l *projectsvc.NewLocale) { l.FlutterDir = "" }},
		{"whitespace-only flutter_dir", func(l *projectsvc.NewLocale) { l.FlutterDir = "   " }},
		{"empty android_values_dir", func(l *projectsvc.NewLocale) { l.AndroidValuesDir = "" }},
		{"empty ios_lproj", func(l *projectsvc.NewLocale) { l.IOSLproj = "" }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := valid()
			c.mutate(&in)
			_, err := svc.AddLocale(ctx, "youtrip", in)
			assert.ErrorIs(t, err, projectsvc.ErrBadRequest)
		})
	}

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM locales WHERE code = 'km-KH'`).Scan(&count))
	assert.Zero(t, count, "no case above is valid; nothing should have been written")
}

// TestAddLocaleWritesToTheNamedProject proves AddLocale resolves the project
// CODE in the path to the right numeric project_id before writing — a wrong
// resolution would either write to project 1 regardless of the path, or fail
// where it should succeed.
func TestAddLocaleWritesToTheNamedProject(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	const actor = "addlocale-target@you.co"
	_, err := testDB.Exec(`INSERT INTO users (email, role) VALUES ($1, 'viewer')`, actor)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testDB.Exec(`DELETE FROM users WHERE email = $1`, actor)
		require.NoError(t, err)
	})

	project, err := svc.Create(ctx, actor, projectsvc.NewProject{
		Code: "addlocale-target", Name: "AddLocale Target",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Asserted, not discarded, like every scope_*_test.go cleanup: locales
		// and grants both reference this project with no ON DELETE action, so a
		// swallowed error here leaks the project AND a second 'km-KH' locale,
		// and the next run fails somewhere else entirely.
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE project_id = $1`, project.ID)
		require.NoError(t, cleanupErr)
		_, cleanupErr = testDB.Exec(`DELETE FROM user_project_roles WHERE project_id = $1`, project.ID)
		require.NoError(t, cleanupErr)
		_, cleanupErr = testDB.Exec(`DELETE FROM projects WHERE id = $1`, project.ID)
		require.NoError(t, cleanupErr)
	})

	created, err := svc.AddLocale(ctx, "addlocale-target", projectsvc.NewLocale{
		Code: "km-KH", FlutterDir: "km_KH",
		AndroidValuesDir: "values-km", IOSLproj: "km-KH.lproj", SortOrder: 9,
	})
	require.NoError(t, err)
	assert.Equal(t, project.ID, created.ProjectID,
		"the locale must belong to the project named in the path, not project 1")
	assert.Equal(t, "active", created.Status, "a new locale defaults to active")

	// project 1 (youtrip) must be untouched: km-KH there would be a leak
	// across the project boundary that ProjectID alone cannot catch if the
	// repository's WHERE clause were wrong in a way that still returned a row.
	var leaked int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM locales WHERE project_id = 1 AND code = 'km-KH'`).Scan(&leaked))
	assert.Zero(t, leaked, "the locale must not also appear on project 1")
}

// TestAddLocalePropagatesDuplicateSentinels proves repository.ErrLocaleCodeTaken
// and ErrLocaleDirectoryTaken pass through AddLocale unwrapped, exactly like
// ErrProjectCodeTaken passes through Create — errors.Is on the sentinel, not
// on a wrapped copy, is what the handler matches.
func TestAddLocalePropagatesDuplicateSentinels(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	_, err := svc.AddLocale(ctx, "youtrip", projectsvc.NewLocale{
		Code: "en-SG", FlutterDir: "en_SG_svc_dup",
		AndroidValuesDir: "values-svc-dup", IOSLproj: "en-SG-svc-dup.lproj", SortOrder: 30,
	})
	assert.ErrorIs(t, err, repository.ErrLocaleCodeTaken, "en-SG already exists on youtrip")

	_, err = svc.AddLocale(ctx, "youtrip", projectsvc.NewLocale{
		Code: "zz-ZZ", FlutterDir: "en_SG", // en-SG's directory, on a fresh code
		AndroidValuesDir: "values-zz-svc-dup", IOSLproj: "zz-ZZ-svc-dup.lproj", SortOrder: 31,
	})
	assert.ErrorIs(t, err, repository.ErrLocaleDirectoryTaken)
}

// TestUpdateLocaleRejectsAnUnknownStatus mirrors
// TestProjectUpdateRejectsAnUnknownStatus: the same "no third state" rule
// applies to a locale's lifecycle.
func TestUpdateLocaleRejectsAnUnknownStatus(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	_, err := svc.UpdateLocale(ctx, "youtrip", "en-SG", projectsvc.LocalePatch{
		FlutterDir: "en_SG", AndroidValuesDir: "values", IOSLproj: "en-SG.lproj",
		Status: "paused",
	})
	assert.ErrorIs(t, err, projectsvc.ErrBadRequest)

	// The rejected patch must not have written anything — status stays what
	// it was.
	var status string
	require.NoError(t, testDB.QueryRow(
		`SELECT status FROM locales WHERE project_id = 1 AND code = 'en-SG'`).Scan(&status))
	assert.Equal(t, "active", status)
}

// TestUpdateLocaleReportsAMissingLocale: a typo'd code, or a code belonging
// to a different project, is a 404 via repository.ErrNotFound.
func TestUpdateLocaleReportsAMissingLocale(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	_, err := svc.UpdateLocale(ctx, "youtrip", "no-such-locale", projectsvc.LocalePatch{
		FlutterDir: "x", AndroidValuesDir: "y", IOSLproj: "z", Status: "active",
	})
	assert.ErrorIs(t, err, repository.ErrNotFound)
}

// TestUpdateLocaleReportsAMissingProject: the project half of the lookup can
// also miss, and must not be confused with a missing locale.
func TestUpdateLocaleReportsAMissingProject(t *testing.T) {
	ctx := context.Background()
	svc := newProjectSvc(t)

	_, err := svc.UpdateLocale(ctx, "no-such-project", "en-SG", projectsvc.LocalePatch{
		FlutterDir: "x", AndroidValuesDir: "y", IOSLproj: "z", Status: "active",
	})
	assert.ErrorIs(t, err, repository.ErrNotFound)
}
