package integrationtests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// TestProjectsSeed pins YouTrip at id 1. V1.10 onward default their
// project_id columns to that literal, so the id is load-bearing rather than
// incidental.
func TestProjectsSeed(t *testing.T) {
	var id int16
	var code, status string
	require.NoError(t, testDB.QueryRow(
		`SELECT id, code, status FROM projects WHERE code = 'youtrip'`).
		Scan(&id, &code, &status))

	assert.Equal(t, int16(1), id, "YouTrip must be project 1")
	assert.Equal(t, "active", status)
}

func TestProjectConstraintsRejectBadInput(t *testing.T) {
	// The status case deliberately carries a VALID code: naming the constraint
	// is what now proves it is projects_status_check doing the refusing rather
	// than projects_code_format_check tripping first on a code chosen to be
	// bad for an unrelated reason.
	cases := []struct {
		name, code, status, constraint string
	}{
		{"duplicate code", "youtrip", "active", "projects_code_unique"},
		{"uppercase code", "YouBiz", "active", "projects_code_format_check"},
		{"code with space", "you biz", "active", "projects_code_format_check"},
		{"code starting with digit", "1biz", "active", "projects_code_format_check"},
		{"empty code", "", "active", "projects_code_format_check"},
		{"unknown status", "youbiz", "paused", "projects_status_check"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := testDB.Exec(
				`INSERT INTO projects (code, name, status) VALUES ($1, $2, $3)`,
				c.code, "Test", c.status)
			requireRejected(t, err, c.constraint, c.name)
		})
	}
}

func TestProjectRepositoryRoundTrip(t *testing.T) {
	repo := repository.ProvideProjectRepository(testGORM(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, nil, model.Project{
		Code: "youbiz", Name: "YouBiz", LokaliseProjectID: "lok-123",
	})
	require.NoError(t, err)
	assert.Greater(t, created.ID, int16(1))
	assert.Equal(t, "active", created.Status)

	byCode, err := repo.ByCode(ctx, nil, "youbiz")
	require.NoError(t, err)
	assert.Equal(t, created.ID, byCode.ID)
	assert.Equal(t, "lok-123", byCode.LokaliseProjectID)

	_, err = repo.Create(ctx, nil, model.Project{Code: "youbiz", Name: "Dup"})
	assert.ErrorIs(t, err, repository.ErrProjectCodeTaken,
		"a duplicate code is the caller's mistake, not a 500")

	_, err = repo.ByCode(ctx, nil, "nope")
	assert.ErrorIs(t, err, repository.ErrNotFound)

	updated, err := repo.Update(ctx, nil, created.ID, "YouBiz SG",
		repository.ProjectArchived, "lok-456")
	require.NoError(t, err)
	assert.Equal(t, "YouBiz SG", updated.Name)

	active, err := repo.List(ctx, nil, false)
	require.NoError(t, err)
	for _, p := range active {
		assert.NotEqual(t, created.ID, p.ID, "archived projects are excluded")
	}

	all, err := repo.List(ctx, nil, true)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	_, err = testDB.Exec(`DELETE FROM projects WHERE id = $1`, created.ID)
	require.NoError(t, err)
}

// TestAddingALocaleWritesNoTranslations is the property that makes arbitrary
// locale counts cheap: absent means untranslated, so a new locale starts
// empty and fills in as translators work. Backfilling empty strings here
// would manufacture thousands of deliberately-blank values, which is exactly
// the collapse the three-state rule forbids.
func TestAddingALocaleWritesNoTranslations(t *testing.T) {
	repo := repository.ProvideLocaleRepository(testGORM(t))
	ctx := context.Background()

	var before int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM translations WHERE project_id = 1`).Scan(&before))

	created, err := repo.Create(ctx, nil, model.Locale{
		ProjectID: 1, Code: "vi-VN", FlutterDir: "vi_VN",
		AndroidValuesDir: "values-vi", IOSLproj: "vi-VN.lproj", SortOrder: 7,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE id = $1`, created.ID)
		require.NoError(t, cleanupErr)
	})

	var after int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM translations WHERE project_id = 1`).Scan(&after))
	assert.Equal(t, before, after, "adding a locale must write no translation rows")

	active, err := repo.List(ctx, nil, 1, false)
	require.NoError(t, err)
	assert.Len(t, active, 7, "the new locale joins the six seeded ones")
}

// TestArchivedLocalesLeaveTheActiveList: archive, never delete — translations
// and history reference the row.
func TestArchivedLocalesLeaveTheActiveList(t *testing.T) {
	repo := repository.ProvideLocaleRepository(testGORM(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, nil, model.Locale{
		ProjectID: 1, Code: "id-ID", FlutterDir: "id_ID",
		AndroidValuesDir: "values-id", IOSLproj: "id-ID.lproj", SortOrder: 8,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE id = $1`, created.ID)
		require.NoError(t, cleanupErr)
	})

	created.Status = "archived"
	_, err = repo.Update(ctx, nil, 1, "id-ID", created)
	require.NoError(t, err)

	active, err := repo.List(ctx, nil, 1, false)
	require.NoError(t, err)
	for _, l := range active {
		assert.NotEqual(t, "id-ID", l.Code, "archived locales leave the active list")
	}

	all, err := repo.List(ctx, nil, 1, true)
	require.NoError(t, err)
	var found bool
	for _, l := range all {
		if l.Code == "id-ID" {
			found = true
		}
	}
	assert.True(t, found, "the row survives archiving")
}

// TestCreatingALocaleWithATakenCodeIsRefused proves the ON CONFLICT (project_id,
// code) DO NOTHING path is reachable and mapped to a typed sentinel, not left
// as a bare "no rows" the caller has no way to distinguish from an unrelated
// failure. en-SG already exists on project 1 (seeded by V1.00).
func TestCreatingALocaleWithATakenCodeIsRefused(t *testing.T) {
	repo := repository.ProvideLocaleRepository(testGORM(t))
	ctx := context.Background()

	_, err := repo.Create(ctx, nil, model.Locale{
		ProjectID: 1, Code: "en-SG", FlutterDir: "en_SG_dup",
		AndroidValuesDir: "values-en-dup", IOSLproj: "en-SG-dup.lproj", SortOrder: 20,
	})
	assert.ErrorIs(t, err, repository.ErrLocaleCodeTaken)
}

// TestCreatingALocaleWithATakenDirectoryIsRefused proves the per-project
// UNIQUE(project_id, flutter_dir) constraint (added in V1.10) is not merely
// present but reachable through Create, and that the resulting driver error
// is mapped to a sentinel rather than surfacing as an opaque 500. en-SG
// (seeded) already owns flutter_dir "en_SG" on project 1; the code here is
// otherwise fine, so a failure isolates the directory collision alone.
func TestCreatingALocaleWithATakenDirectoryIsRefused(t *testing.T) {
	repo := repository.ProvideLocaleRepository(testGORM(t))
	ctx := context.Background()

	_, err := repo.Create(ctx, nil, model.Locale{
		ProjectID: 1, Code: "zz-ZZ", FlutterDir: "en_SG",
		AndroidValuesDir: "values-zz-unique", IOSLproj: "zz-ZZ-unique.lproj", SortOrder: 21,
	})
	assert.ErrorIs(t, err, repository.ErrLocaleDirectoryTaken)
}

// TestUpdatingALocaleCannotChangeItsCode proves the repository's Update
// signature makes the code un-writable by construction: it is a WHERE
// predicate, not a SET target, so there is no way to pass a new code through
// this method at all. Renaming would silently orphan every translation and
// history row that references the locale by its old identity.
func TestUpdatingALocaleCannotChangeItsCode(t *testing.T) {
	repo := repository.ProvideLocaleRepository(testGORM(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, nil, model.Locale{
		ProjectID: 1, Code: "km-KH", FlutterDir: "km_KH",
		AndroidValuesDir: "values-km", IOSLproj: "km-KH.lproj", SortOrder: 22,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE id = $1`, created.ID)
		require.NoError(t, cleanupErr)
	})

	// Update is addressed by the OLD code; there is no field in model.Locale
	// that Update writes to the code column at all.
	updated, err := repo.Update(ctx, nil, 1, "km-KH", model.Locale{
		FlutterDir: "km_KH_2", AndroidValuesDir: "values-km-2",
		IOSLproj: "km-KH-2.lproj", SortOrder: 23, Status: "active",
	})
	require.NoError(t, err)
	assert.Equal(t, "km-KH", updated.Code, "the code must survive an update unchanged")

	byCode, err := repo.ByCode(ctx, nil, 1, "km-KH")
	require.NoError(t, err)
	assert.Equal(t, "km_KH_2", byCode.FlutterDir, "the other fields did write")
}

// TestUpdatingAMissingLocaleReportsNotFound: a typo'd code, or a code that
// belongs to a different project, must not be confused with success.
func TestUpdatingAMissingLocaleReportsNotFound(t *testing.T) {
	repo := repository.ProvideLocaleRepository(testGORM(t))
	ctx := context.Background()

	_, err := repo.Update(ctx, nil, 1, "no-such-locale", model.Locale{
		FlutterDir: "x", AndroidValuesDir: "y", IOSLproj: "z", Status: "active",
	})
	assert.ErrorIs(t, err, repository.ErrNotFound)
}
