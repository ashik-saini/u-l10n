package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// ErrProjectCodeTaken is returned when a code is already in use. Exported so
// the handler can answer 409 rather than letting a unique violation surface
// as an opaque 500.
var ErrProjectCodeTaken = errors.New("project code already in use")

// Project statuses. A project is archived, never deleted: releases, history
// and audit rows reference it.
const (
	ProjectActive   = "active"
	ProjectArchived = "archived"
)

// ProjectRepository owns the projects table.
//
// Every method takes an optional tx: creating a project and granting its
// first role must land together or not at all.
type ProjectRepository interface {
	ByCode(ctx context.Context, tx *gorm.DB, code string) (model.Project, error)
	ByID(ctx context.Context, tx *gorm.DB, id int16) (model.Project, error)
	// List returns projects in code order. Archived projects are excluded
	// unless asked for, because every caller but the admin screen wants the
	// live ones.
	List(ctx context.Context, tx *gorm.DB, includeArchived bool) ([]model.Project, error)
	Create(ctx context.Context, tx *gorm.DB, p model.Project) (model.Project, error)
	Update(ctx context.Context, tx *gorm.DB, id int16, name, status, lokaliseProjectID string) (model.Project, error)
}

type projectRepository struct{ base }

func ProvideProjectRepository(connector database.GORMConnector) ProjectRepository {
	return &projectRepository{base{connector: connector}}
}

// COALESCE keeps model.Project free of sql.NullString: absent and empty are
// the same fact for a Lokalise id, unlike a translation value.
const projectColumns = `id, code, name, status,
	COALESCE(lokalise_project_id, ''), created_at, updated_at`

func scanProject(row interface{ Scan(...interface{}) error }) (model.Project, error) {
	var p model.Project
	err := row.Scan(&p.ID, &p.Code, &p.Name, &p.Status,
		&p.LokaliseProjectID, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (r *projectRepository) ByCode(ctx context.Context, tx *gorm.DB, code string) (model.Project, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+projectColumns+` FROM projects WHERE code = ?`, code).Row()

	p, err := scanProject(row)
	switch {
	case err == nil:
		return p, nil
	case isNoRows(err):
		return model.Project{}, fmt.Errorf("project %q: %w", code, ErrNotFound)
	default:
		return model.Project{}, fmt.Errorf("read project %q: %w", code, err)
	}
}

func (r *projectRepository) ByID(ctx context.Context, tx *gorm.DB, id int16) (model.Project, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id).Row()

	p, err := scanProject(row)
	switch {
	case err == nil:
		return p, nil
	case isNoRows(err):
		return model.Project{}, fmt.Errorf("project %d: %w", id, ErrNotFound)
	default:
		return model.Project{}, fmt.Errorf("read project %d: %w", id, err)
	}
}

func (r *projectRepository) List(ctx context.Context, tx *gorm.DB, includeArchived bool) ([]model.Project, error) {
	q := `SELECT ` + projectColumns + ` FROM projects`
	if !includeArchived {
		q += ` WHERE status = '` + ProjectActive + `'`
	}
	q += ` ORDER BY code`

	rows, err := r.db(ctx, tx).Raw(q).Rows()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *projectRepository) Create(ctx context.Context, tx *gorm.DB, p model.Project) (model.Project, error) {
	status := p.Status
	if status == "" {
		status = ProjectActive
	}
	row := r.db(ctx, tx).Raw(
		`INSERT INTO projects (code, name, status, lokalise_project_id)
		 VALUES (?, ?, ?, NULLIF(?, ''))
		 ON CONFLICT (code) DO NOTHING
		 RETURNING `+projectColumns,
		p.Code, p.Name, status, p.LokaliseProjectID).Row()

	created, err := scanProject(row)
	switch {
	case err == nil:
		return created, nil
	case isNoRows(err):
		// DO NOTHING returned no row: the code is taken.
		return model.Project{}, fmt.Errorf("create project %q: %w", p.Code, ErrProjectCodeTaken)
	default:
		return model.Project{}, fmt.Errorf("create project %q: %w", p.Code, err)
	}
}

func (r *projectRepository) Update(ctx context.Context, tx *gorm.DB, id int16, name, status, lokaliseProjectID string) (model.Project, error) {
	row := r.db(ctx, tx).Raw(
		`UPDATE projects
		    SET name = ?, status = ?, lokalise_project_id = NULLIF(?, ''),
		        updated_at = now()
		  WHERE id = ?
		 RETURNING `+projectColumns,
		name, status, lokaliseProjectID, id).Row()

	updated, err := scanProject(row)
	switch {
	case err == nil:
		return updated, nil
	case isNoRows(err):
		return model.Project{}, fmt.Errorf("project %d: %w", id, ErrNotFound)
	default:
		return model.Project{}, fmt.Errorf("update project %d: %w", id, err)
	}
}
