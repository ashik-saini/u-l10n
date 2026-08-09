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
	cases := []struct {
		name, code, status string
	}{
		{"duplicate code", "youtrip", "active"},
		{"uppercase code", "YouBiz", "active"},
		{"code with space", "you biz", "active"},
		{"code starting with digit", "1biz", "active"},
		{"empty code", "", "active"},
		{"unknown status", "youbiz", "paused"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := testDB.Exec(
				`INSERT INTO projects (code, name, status) VALUES ($1, $2, $3)`,
				c.code, "Test", c.status)
			requireRejected(t, err, c.name)
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
