# Multi-Project Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give u-l10n a project dimension in the database, plus runtime management of projects and their locales, without changing any existing behaviour for the YouTrip data already in it.

**Architecture:** A new `projects` table becomes the root. Tables that own identity (`locales`, `keys`, `tags`, `branches`, `releases`, `assets`, `api_tokens`, and the operational tables) gain a `project_id` column; child tables gain one too and reference their parents through *composite* foreign keys `(project_id, id)`, so a row pairing one project's key with another project's locale is refused by PostgreSQL. Every new column carries a temporary `DEFAULT 1` (the seeded YouTrip project) so existing INSERT statements keep working unchanged — Plan 2 removes those defaults as it makes scope explicit in code.

**Tech Stack:** Go 1.26, GORM v1 with hand-written SQL, PostgreSQL, Flyway migrations, chi v4, Google Wire, testify + testcontainers.

## Global Constraints

Copied from `docs/superpowers/specs/2026-08-09-multi-project-design.md` and `CLAUDE.md`. Every task inherits these.

- Migrations are `.db/V1.NN__description.sql`, zero-padded so lexical and version order agree. Forward-only. Each gets a companion `U1.NN__` file that is **documentation only and never executed**.
- GORM v1 has no `clause.OnConflict` and no `clause.Locking`. Use hand-written SQL through `Exec`/`Raw` for `ON CONFLICT`, and `Set("gorm:query_option", "FOR UPDATE")` for row locks.
- Repositories never open a transaction. Every method takes an optional `tx *gorm.DB` and resolves the handle with `r.db(ctx, tx)`. Services own transaction boundaries.
- Never return a bare `string` for a translation value. Absent, empty, and translated are three distinct states.
- `pkg/model` imports the standard library only.
- Errors are exported sentinels matched with `errors.Is` and mapped to HTTP status in the handler. Caller mistakes are 4xx; only genuinely unexpected failures reach 500.
- Tests use testify. Integration tests must prove constraints **fail**, not merely that inserts succeed. `requireRejected` demands a class-23 SQLSTATE.
- Run `gofmt -w . && go build ./... && go vet ./... && go test -race ./...` before every commit.
- Never edit `wire_gen.go` by hand. Regenerate with `make gen-wire`, which runs `wire .` (not `wire ./...`).
- The seeded YouTrip project is `id = 1`, inserted explicitly so the temporary column defaults are deterministic.

---

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `.db/V1.09__projects.sql` | `projects` table, seed YouTrip as id 1 |
| `.db/V1.10__scope_core.sql` | `project_id` on locales, keys, translations, tags, key_tags |
| `.db/V1.11__scope_workflow.sql` | `project_id` on branches, branch deltas, merge requests, resolutions |
| `.db/V1.12__scope_releases_assets.sql` | `project_id` on releases, bundles, assets, key_assets |
| `.db/V1.13__scope_identity.sql` | `user_project_roles`, platform admin flag, token/settings/audit/history scoping |
| `.db/U1.09`–`U1.13` | Undo documentation, never executed |
| `pkg/repository/project.go` | The only place that speaks SQL about projects |
| `pkg/service/projectsvc/projectsvc.go` | Project and locale management rules |
| `route/project.go` | Project and locale admin handlers |
| `integration-tests/project_test.go` | Schema constraints and repository behaviour |
| `integration-tests/scope_constraints_test.go` | Cross-project foreign keys must refuse |

**Modified:** `pkg/model/model.go` (Project type, Locale gains ProjectID), `pkg/repository/repository.go` (wire set), `pkg/repository/locale.go` (project-scoped reads and writes), `route/route.go` (admin routes), `main.go` (bootstrap commands), `inject_service.go` (providers).

---

### Task 1: The projects table and repository

**Files:**
- Create: `.db/V1.09__projects.sql`, `.db/U1.09__projects.sql`, `pkg/repository/project.go`, `integration-tests/project_test.go`
- Modify: `pkg/model/model.go`, `pkg/repository/repository.go`

**Interfaces:**
- Consumes: nothing — this is the root task.
- Produces: `model.Project`; `repository.ProjectRepository` with `ByCode(ctx, tx, code string) (model.Project, error)`, `ByID(ctx, tx, id int16) (model.Project, error)`, `List(ctx, tx, includeArchived bool) ([]model.Project, error)`, `Create(ctx, tx, p model.Project) (model.Project, error)`, `Update(ctx, tx, id int16, name, status, lokaliseProjectID string) (model.Project, error)`; sentinel `repository.ErrProjectCodeTaken`; constants `repository.ProjectActive`, `repository.ProjectArchived`.

- [ ] **Step 1: Write the migration**

Create `.db/V1.09__projects.sql`:

```sql
-- The project dimension.
--
-- u-l10n began serving one product. It now serves several — YouTrip and
-- YouBiz today — whose copy is entirely independent: a key named
-- `login_title` in one has nothing to do with the same name in the other.
-- This table is the root that every other row hangs from.
--
-- `code` appears in every URL path, so it is constrained to a slug rather
-- than validated only in the handler. A project is archived, never deleted:
-- releases, history and audit rows reference it, and V1.08 already settled
-- that an audit record outlives what it describes.
--
-- YouTrip is inserted with an EXPLICIT id of 1, and the sequence advanced to
-- match. V1.10 onward give each new project_id column a temporary
-- `DEFAULT 1` so that the existing INSERT statements — which know nothing
-- about projects yet — keep working until the code passes the scope
-- explicitly. Those defaults are dropped once it does.
CREATE TABLE IF NOT EXISTS projects (
    id                  SMALLSERIAL PRIMARY KEY,
    code                TEXT NOT NULL,
    name                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',
    lokalise_project_id TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT projects_code_unique UNIQUE (code),
    CONSTRAINT projects_code_format_check CHECK (code ~ '^[a-z][a-z0-9-]{1,31}$'),
    CONSTRAINT projects_status_check CHECK (status IN ('active', 'archived'))
);

INSERT INTO projects (id, code, name) VALUES (1, 'youtrip', 'YouTrip')
ON CONFLICT (code) DO NOTHING;

SELECT setval(pg_get_serial_sequence('projects', 'id'),
              (SELECT max(id) FROM projects));
```

Create `.db/U1.09__projects.sql`:

```sql
-- Undo for V1.09. Never executed; see U1.00.
--
-- Dropping this table drops the dimension every other table hangs from, so it
-- can only run after U1.10-U1.13 have removed their project_id columns.
DROP TABLE IF EXISTS projects;
```

- [ ] **Step 2: Write the failing schema test**

Create `integration-tests/project_test.go`:

```go
package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestProject' -v`
Expected: FAIL — `relation "projects" does not exist`.

- [ ] **Step 4: Apply the migration and re-run**

Run: `make db-test && REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestProject' -v`
Expected: PASS. If `TestProjectsSeed` reports an id other than 1, the scratch database was not rebuilt from empty — rerun `make db-migrate`.

- [ ] **Step 5: Add the model type**

In `pkg/model/model.go`, alongside the existing types:

```go
// Project is the root of the ownership tree. Every key, locale, branch and
// release belongs to exactly one. Codes appear in URLs, so they are slugs.
type Project struct {
	ID                int16
	Code              string
	Name              string
	Status            string
	LokaliseProjectID string // empty when the project has no Lokalise source
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
```

- [ ] **Step 6: Write the failing repository test**

Append to `integration-tests/project_test.go`:

```go
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
```

Add `"context"`, `"github.com/yougroupteam/u-l10n/pkg/model"` and `"github.com/yougroupteam/u-l10n/pkg/repository"` to the imports.

- [ ] **Step 7: Run the test to verify it fails**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestProjectRepositoryRoundTrip' -v`
Expected: FAIL — `undefined: repository.ProvideProjectRepository`.

- [ ] **Step 8: Implement the repository**

Create `pkg/repository/project.go`:

```go
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
	if isNoRows(err) {
		return model.Project{}, ErrNotFound
	}
	if err != nil {
		return model.Project{}, fmt.Errorf("project by code: %w", err)
	}
	return p, nil
}

func (r *projectRepository) ByID(ctx context.Context, tx *gorm.DB, id int16) (model.Project, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id).Row()
	p, err := scanProject(row)
	if isNoRows(err) {
		return model.Project{}, ErrNotFound
	}
	if err != nil {
		return model.Project{}, fmt.Errorf("project by id: %w", err)
	}
	return p, nil
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
	if isNoRows(err) {
		// DO NOTHING returned no row: the code is taken.
		return model.Project{}, ErrProjectCodeTaken
	}
	if err != nil {
		return model.Project{}, fmt.Errorf("create project: %w", err)
	}
	return created, nil
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
	if isNoRows(err) {
		return model.Project{}, ErrNotFound
	}
	if err != nil {
		return model.Project{}, fmt.Errorf("update project: %w", err)
	}
	return updated, nil
}
```

Before writing this, confirm the existing no-rows helper's name:

Run: `grep -rn "func isNoRows" pkg/repository/`
If it is named differently, use the existing name — do not add a second one.

- [ ] **Step 9: Register the provider**

In `pkg/repository/repository.go`, add `ProvideProjectRepository,` to `WireSet`.

Run: `make gen-wire`
Expected: `wire_gen.go` regenerates without error. Never edit it by hand.

- [ ] **Step 10: Run the tests to verify they pass**

Run: `gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestProject' -v`
Expected: PASS for all three tests.

- [ ] **Step 11: Commit**

```bash
git add .db/V1.09__projects.sql .db/U1.09__projects.sql pkg/repository/project.go pkg/repository/repository.go pkg/model/model.go integration-tests/project_test.go wire_gen.go
git commit -m "feat(db): add the projects table and its repository"
```

---

### Task 2: Scope the core tables

**Files:**
- Create: `.db/V1.10__scope_core.sql`, `.db/U1.10__scope_core.sql`, `integration-tests/scope_constraints_test.go`
- Modify: `pkg/model/model.go` (Locale gains ProjectID), `pkg/repository/locale.go`

**Interfaces:**
- Consumes: `projects` seeded with YouTrip at id 1 (Task 1).
- Produces: `project_id` on `locales`, `keys`, `translations`, `tags`, `key_tags`; composite keys `keys (project_id, id)` and `locales (project_id, id)` available as foreign-key targets for Tasks 3 and 4; `model.Locale.ProjectID`; `LocaleRepository.List` and `ByCode` take a `projectID int16` argument.

- [ ] **Step 1: Confirm the existing foreign-key constraint names**

Run:

```bash
psql "$DATABASECONFIG_URL" -c "SELECT conrelid::regclass AS tbl, conname FROM pg_constraint WHERE contype = 'f' AND conrelid::regclass::text IN ('translations','key_tags') ORDER BY 1,2;"
```

Expected: names of the form `translations_key_id_fkey`. Use whatever names this prints in the migration below — the `DROP CONSTRAINT` statements must match exactly or the old single-column keys survive alongside the new composite ones.

- [ ] **Step 2: Write the migration**

Create `.db/V1.10__scope_core.sql`:

```sql
-- Scope the core matrix to a project.
--
-- THE COMPOSITE FOREIGN KEYS ARE THE POINT. A single-column key
-- (translations.key_id -> keys.id) cannot express "and they must belong to
-- the same project", so nothing but a reviewer's attention would stop a
-- YouTrip key being paired with a YouBiz locale. Since locale SETS now differ
-- per project, that pairing is not hypothetical. Referencing
-- keys (project_id, id) makes the illegal row unrepresentable.
--
-- THE TEMPORARY DEFAULT. Every column below is created with DEFAULT 1 — the
-- YouTrip project seeded in V1.09 — so that today's INSERT statements, which
-- know nothing about projects, keep working. Plan 2 passes the scope
-- explicitly and drops these defaults; until then the default is what keeps
-- the service running.
ALTER TABLE locales ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE locales SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE locales
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT locales_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT locales_project_id_unique UNIQUE (project_id, id);

-- The code is unique WITHIN a project now: YouBiz may ship its own en-SG.
ALTER TABLE locales DROP CONSTRAINT locales_code_unique;
ALTER TABLE locales ADD CONSTRAINT locales_project_code_unique UNIQUE (project_id, code);

-- Two locales in one project must not claim the same export directory.
-- Without this an admin could aim both at `values/` and the export zip would
-- silently write one over the other — a data-integrity failure the database
-- should refuse rather than a reviewer catch.
ALTER TABLE locales
    ADD CONSTRAINT locales_project_flutter_dir_unique UNIQUE (project_id, flutter_dir),
    ADD CONSTRAINT locales_project_android_dir_unique UNIQUE (project_id, android_values_dir),
    ADD CONSTRAINT locales_project_ios_lproj_unique   UNIQUE (project_id, ios_lproj);

-- Locales become admin-managed data rather than migration-seeded reference
-- data, so they need a lifecycle. Archived, never deleted: translations and
-- history reference them.
ALTER TABLE locales ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE locales ADD CONSTRAINT locales_status_check CHECK (status IN ('active', 'archived'));

ALTER TABLE keys ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE keys SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE keys
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT keys_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT keys_project_id_unique UNIQUE (project_id, id);

-- Name uniqueness among ACTIVE keys becomes per project. The partial
-- predicate is unchanged: a soft-deleted key must not block reuse of its name.
DROP INDEX IF EXISTS idx_keys_name_active;
CREATE UNIQUE INDEX IF NOT EXISTS idx_keys_name_active
    ON keys (project_id, name) WHERE status = 'active';

-- The import anchor is per project: each project imports from its own
-- Lokalise project, whose key ids are its own numbering.
ALTER TABLE keys DROP CONSTRAINT keys_lokalise_key_id_unique;
ALTER TABLE keys ADD CONSTRAINT keys_project_lokalise_key_unique UNIQUE (project_id, lokalise_key_id);

ALTER TABLE translations ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE translations SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE translations
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE translations DROP CONSTRAINT translations_key_id_fkey;
ALTER TABLE translations DROP CONSTRAINT translations_locale_id_fkey;
ALTER TABLE translations
    ADD CONSTRAINT translations_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT translations_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE tags ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE tags SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE tags
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT tags_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT tags_project_id_unique UNIQUE (project_id, id);

ALTER TABLE tags DROP CONSTRAINT tags_name_unique;
ALTER TABLE tags ADD CONSTRAINT tags_project_name_unique UNIQUE (project_id, name);

ALTER TABLE key_tags ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE key_tags SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE key_tags
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE key_tags DROP CONSTRAINT key_tags_key_id_fkey;
ALTER TABLE key_tags DROP CONSTRAINT key_tags_tag_id_fkey;
ALTER TABLE key_tags
    ADD CONSTRAINT key_tags_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT key_tags_tag_fkey
        FOREIGN KEY (project_id, tag_id) REFERENCES tags (project_id, id) ON DELETE CASCADE;

-- The key browse is the portal's busiest query and now filters on project
-- first. Leading the index with project_id keeps it a range scan rather than
-- a filter over every project's keys.
DROP INDEX IF EXISTS idx_keys_status_name;
CREATE INDEX IF NOT EXISTS idx_keys_project_status_name ON keys (project_id, status, name);
```

Create `.db/U1.10__scope_core.sql`:

```sql
-- Undo for V1.10. Never executed; see U1.00.
--
-- Reverting drops the composite foreign keys, which are the only thing
-- preventing a translation from pairing one project's key with another
-- project's locale. With more than one project present that is immediate
-- silent corruption, not a return to a previous working state.
ALTER TABLE key_tags DROP CONSTRAINT key_tags_key_fkey, DROP CONSTRAINT key_tags_tag_fkey;
ALTER TABLE key_tags
    ADD CONSTRAINT key_tags_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE,
    ADD CONSTRAINT key_tags_tag_id_fkey FOREIGN KEY (tag_id) REFERENCES tags (id) ON DELETE CASCADE;
ALTER TABLE key_tags DROP COLUMN project_id;

ALTER TABLE translations DROP CONSTRAINT translations_key_fkey, DROP CONSTRAINT translations_locale_fkey;
ALTER TABLE translations
    ADD CONSTRAINT translations_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE,
    ADD CONSTRAINT translations_locale_id_fkey FOREIGN KEY (locale_id) REFERENCES locales (id);
ALTER TABLE translations DROP COLUMN project_id;

ALTER TABLE tags DROP CONSTRAINT tags_project_name_unique;
ALTER TABLE tags ADD CONSTRAINT tags_name_unique UNIQUE (name);
ALTER TABLE tags DROP COLUMN project_id;

DROP INDEX IF EXISTS idx_keys_name_active;
CREATE UNIQUE INDEX idx_keys_name_active ON keys (name) WHERE status = 'active';
ALTER TABLE keys DROP CONSTRAINT keys_project_lokalise_key_unique;
ALTER TABLE keys ADD CONSTRAINT keys_lokalise_key_id_unique UNIQUE (lokalise_key_id);
ALTER TABLE keys DROP COLUMN project_id;

ALTER TABLE locales DROP CONSTRAINT locales_project_code_unique;
ALTER TABLE locales ADD CONSTRAINT locales_code_unique UNIQUE (code);
ALTER TABLE locales DROP COLUMN project_id, DROP COLUMN status;
```

- [ ] **Step 3: Write the failing constraint test**

Create `integration-tests/scope_constraints_test.go`:

```go
package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCrossProjectPairingIsRefused proves the composite foreign keys do the
// work the design assigns them. These INSERTs are the corruption the whole
// scoping scheme exists to prevent, so the test asserts they FAIL — a test
// that only proved valid rows insert would pass just as happily against a
// schema with no constraints at all.
func TestCrossProjectPairingIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('scopetest', 'Scope Test')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	// A locale belonging to the other project.
	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))

	// A key belonging to YouTrip.
	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status)
		 VALUES (1, 'scope.test.key', '{"flutter"}', 'active')
		 RETURNING id`).Scan(&youtripKey))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
	})

	t.Run("translation pairing across projects", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO translations (project_id, key_id, locale_id, value, version)
			 VALUES (1, $1, $2, 'x', 1)`, youtripKey, otherLocale)
		requireRejected(t, err, "YouTrip key with another project's locale")
	})

	t.Run("translation claiming the wrong project", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO translations (project_id, key_id, locale_id, value, version)
			 VALUES ($1, $2, $3, 'x', 1)`, otherProject, youtripKey, otherLocale)
		requireRejected(t, err, "another project claiming a YouTrip key")
	})
}

// TestLocaleExportDirectoriesAreUniquePerProject: two locales aiming at one
// directory would make the export zip silently overwrite one with the other.
func TestLocaleExportDirectoriesAreUniquePerProject(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES (1, 'xx-XX', 'en_SG', 'values-xx', 'xx.lproj', 99)`)
	requireRejected(t, err, "duplicate flutter_dir within a project")

	_, err = testDB.Exec(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES (1, 'yy-YY', 'yy_YY', 'values', 'yy.lproj', 99)`)
	requireRejected(t, err, "duplicate android_values_dir within a project")
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestCrossProject|TestLocaleExport' -v`
Expected: FAIL — `column "project_id" of relation "locales" does not exist`.

- [ ] **Step 5: Apply the migration and re-run**

Run: `make db-test && REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestCrossProject|TestLocaleExport' -v`
Expected: PASS. A failure reading "insert succeeded" means a `DROP CONSTRAINT` name in Step 2 did not match reality — recheck Step 1.

- [ ] **Step 6: Scope the locale model and repository**

In `pkg/model/model.go`, add `ProjectID int16` and `Status string` to `Locale`.

In `pkg/repository/locale.go`, change the interface and both queries. The doc comment on `LocaleRepository` currently says locales "are seeded by migration V1.00 and are not writable at runtime" — that is no longer true, so replace it:

```go
// LocaleRepository reads and writes the locale dimension.
//
// Locales are per project and admin-managed: adding one to a project must be
// an action in the portal, not a migration and a deploy. Adding a locale
// writes no translation rows — absent means untranslated, so a new locale
// starts empty and fills in as translators work.
type LocaleRepository interface {
	List(ctx context.Context, tx *gorm.DB, projectID int16, includeArchived bool) ([]model.Locale, error)
	ByCode(ctx context.Context, tx *gorm.DB, projectID int16, code string) (model.Locale, error)
}
```

Update both statements to select `project_id, status` and filter:

```go
const localeColumns = `id, project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order, status`
```

`List` becomes `WHERE project_id = ?` plus `AND status = 'active'` unless `includeArchived`, ordered by `sort_order`. `ByCode` becomes `WHERE project_id = ? AND code = ?`. Add the two new fields to every `rows.Scan` call in the file.

- [ ] **Step 7: Fix the callers the compiler finds**

Run: `go build ./... 2>&1 | head -40`

Every caller of `List` or `ByCode` now fails to compile. Pass `1` (the YouTrip project) at each call site for now, with this comment at each:

```go
// TODO(plan-2): the scope arrives from the request path once routes are
// project-prefixed. Hardcoded to YouTrip until then.
```

This is the one place in these plans where a TODO is correct: it marks a deliberate bridge with a named successor task, and Plan 2's first step is to grep for it.

- [ ] **Step 8: Verify the whole suite still passes**

Run: `gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./...`
Expected: PASS everywhere, including `TestLocalesSeed` — the six YouTrip locales are untouched by the backfill.

- [ ] **Step 9: Commit**

```bash
git add .db/V1.10__scope_core.sql .db/U1.10__scope_core.sql pkg/model/model.go pkg/repository/locale.go integration-tests/scope_constraints_test.go
git add -u
git commit -m "feat(db): scope locales, keys, translations and tags to a project"
```

---

### Task 3: Scope the workflow tables

**Files:**
- Create: `.db/V1.11__scope_workflow.sql`, `.db/U1.11__scope_workflow.sql`
- Create: `integration-tests/scope_workflow_test.go`

**Interfaces:**
- Consumes: `keys (project_id, id)` and `locales (project_id, id)` from Task 2.
- Produces: `project_id` on `branches`, `branch_translations`, `branch_keys`, `merge_requests`, `merge_conflict_resolutions`; composite key `branches (project_id, id)`.

- [ ] **Step 1: Confirm the existing foreign-key names**

Run:

```bash
psql "$DATABASECONFIG_URL" -c "SELECT conrelid::regclass AS tbl, conname FROM pg_constraint WHERE contype='f' AND conrelid::regclass::text IN ('branch_translations','branch_keys','merge_requests','merge_conflict_resolutions') ORDER BY 1,2;"
```

Use the printed names in the migration below.

- [ ] **Step 2: Write the migration**

Create `.db/V1.11__scope_workflow.sql`:

```sql
-- Scope branches and merge requests to a project.
--
-- Branch names become unique WITHIN a project, so both teams may run a
-- `q3-copy` without one blocking the other. The branch delta tables reference
-- keys and locales through composite keys for the same reason V1.10 gave: a
-- delta must not name another project's key.
--
-- merge_requests carries project_id although it could be reached through its
-- branch. The merge transaction filters on it directly and the advisory lock
-- is derived from it; a join on every one of those statements would be cost
-- with no benefit.
ALTER TABLE branches ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE branches SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE branches
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT branches_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT branches_project_id_unique UNIQUE (project_id, id);

ALTER TABLE branches DROP CONSTRAINT branches_name_unique;
ALTER TABLE branches ADD CONSTRAINT branches_project_name_unique UNIQUE (project_id, name);

ALTER TABLE branch_translations ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE branch_translations SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE branch_translations
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE branch_translations DROP CONSTRAINT branch_translations_branch_id_fkey;
ALTER TABLE branch_translations DROP CONSTRAINT branch_translations_key_id_fkey;
ALTER TABLE branch_translations DROP CONSTRAINT branch_translations_locale_id_fkey;
ALTER TABLE branch_translations
    ADD CONSTRAINT branch_translations_branch_fkey
        FOREIGN KEY (project_id, branch_id) REFERENCES branches (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_translations_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_translations_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE branch_keys ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE branch_keys SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE branch_keys
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE branch_keys DROP CONSTRAINT branch_keys_branch_id_fkey;
ALTER TABLE branch_keys DROP CONSTRAINT branch_keys_key_id_fkey;
ALTER TABLE branch_keys
    ADD CONSTRAINT branch_keys_branch_fkey
        FOREIGN KEY (project_id, branch_id) REFERENCES branches (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_keys_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE;

ALTER TABLE merge_requests ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE merge_requests SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE merge_requests
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE merge_requests DROP CONSTRAINT merge_requests_branch_id_fkey;
ALTER TABLE merge_requests
    ADD CONSTRAINT merge_requests_branch_fkey
        FOREIGN KEY (project_id, branch_id) REFERENCES branches (project_id, id) ON DELETE CASCADE;

ALTER TABLE merge_conflict_resolutions ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE merge_conflict_resolutions SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE merge_conflict_resolutions
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE merge_conflict_resolutions DROP CONSTRAINT merge_conflict_resolutions_key_id_fkey;
ALTER TABLE merge_conflict_resolutions
    ADD CONSTRAINT merge_conflict_resolutions_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE;
```

`merge_request_events` gets no column: it references nothing but its merge request, so there is no cross-project pairing to prevent, and adding one would be a denormalisation nobody reads.

Create `.db/U1.11__scope_workflow.sql` following U1.10's shape: drop each composite constraint, restore the single-column equivalent with its original `ON DELETE` behaviour, drop the column, and restore `branches_name_unique`. Open the file with the same warning U1.10 carries — reverting reintroduces cross-project pairing.

- [ ] **Step 3: Write the failing constraint test**

Create `integration-tests/scope_workflow_test.go` (package `integrationtests`, importing `testing`, `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require`). Tasks 3, 4 and 5 run in parallel, so each owns its own test file — appending to a shared one would collide:

```go
// TestCrossProjectBranchDeltaIsRefused: a branch may only carry deltas for
// keys in its own project.
func TestCrossProjectBranchDeltaIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('branchscope', 'Branch Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	var otherBranch int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES ($1, 'q3-copy', 'open', 'test@you.co') RETURNING id`,
		otherProject).Scan(&otherBranch))

	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status)
		 VALUES (1, 'branchscope.key', '{"flutter"}', 'active') RETURNING id`).
		Scan(&youtripKey))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
	})

	_, err := testDB.Exec(
		`INSERT INTO branch_keys (project_id, branch_id, key_id, name, status, base_master_version)
		 VALUES ($1, $2, $3, 'branchscope.key', 'active', 0)`,
		otherProject, otherBranch, youtripKey)
	requireRejected(t, err, "branch delta naming another project's key")
}

// TestBranchNamesAreUniquePerProjectNotGlobally: both teams may run a q3-copy.
func TestBranchNamesAreUniquePerProjectNotGlobally(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('namescope', 'Name Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	_, err := testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'shared-name', 'open', 'test@you.co')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM branches WHERE name = 'shared-name'`)
	})

	_, err = testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES ($1, 'shared-name', 'open', 'test@you.co')`, otherProject)
	require.NoError(t, err, "the same branch name in another project must be allowed")

	_, err = testDB.Exec(
		`INSERT INTO branches (project_id, name, status, created_by)
		 VALUES (1, 'shared-name', 'open', 'test@you.co')`)
	requireRejected(t, err, "duplicate branch name within one project")
}
```

Confirm the `branches` column list against `.db/V1.02__branches_and_merge_requests.sql` before running — if `created_by` is named differently or other columns are `NOT NULL` without defaults, adjust the INSERTs.

- [ ] **Step 4: Run the tests to verify they fail**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestCrossProjectBranch|TestBranchNamesAre' -v`
Expected: FAIL — `column "project_id" of relation "branches" does not exist`.

- [ ] **Step 5: Apply the migration and re-run**

Run: `make db-test && REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestCrossProjectBranch|TestBranchNamesAre' -v`
Expected: PASS.

- [ ] **Step 6: Verify the whole suite**

Run: `gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./...`
Expected: PASS, including every merge and branch test — the defaults keep existing writes working.

- [ ] **Step 7: Commit**

```bash
git add .db/V1.11__scope_workflow.sql .db/U1.11__scope_workflow.sql integration-tests/scope_constraints_test.go
git commit -m "feat(db): scope branches and merge requests to a project"
```

---

### Task 4: Scope releases, bundles and assets

**Files:**
- Create: `.db/V1.12__scope_releases_assets.sql`, `.db/U1.12__scope_releases_assets.sql`
- Create: `integration-tests/scope_releases_test.go`

**Interfaces:**
- Consumes: `locales (project_id, id)`, `keys (project_id, id)` from Task 2.
- Produces: `project_id` on `releases`, `release_bundles`, `assets`, `key_assets`; release version uniqueness becomes `(project_id, version)`; the OTA servable index leads with `project_id`.

- [ ] **Step 1: Write the migration**

Create `.db/V1.12__scope_releases_assets.sql`:

```sql
-- Scope releases and assets to a project.
--
-- RELEASE VERSIONS RESTART PER PROJECT. YouBiz's first release is 1, not
-- YouTrip's next number. Each app therefore has an independent version line,
-- which is what makes a per-project min_app_version floor meaningful.
--
-- ASSETS ARE ISOLATED RATHER THAN SHARED. Content addressing is preserved
-- within a project by putting the project in the S3 key prefix. Identical
-- bytes uploaded to two projects are stored twice — negligible for
-- screenshots, and it buys a permission model that is a column rather than a
-- join through key_assets, plus isolation at the storage layer.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE releases SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE releases
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT releases_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT releases_project_id_unique UNIQUE (project_id, id);

ALTER TABLE releases DROP CONSTRAINT releases_version_unique;
ALTER TABLE releases ADD CONSTRAINT releases_project_version_unique UNIQUE (project_id, version);

-- The OTA servable lookup is the hottest read in the service and now filters
-- on project first. Leading the index with project_id keeps it a LIMIT 1
-- index scan as projects accumulate.
DROP INDEX IF EXISTS idx_releases_servable;
CREATE INDEX IF NOT EXISTS idx_releases_servable
    ON releases (project_id, version DESC) WHERE rolled_back_at IS NULL;

ALTER TABLE release_bundles ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE release_bundles SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE release_bundles
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE release_bundles DROP CONSTRAINT release_bundles_release_id_fkey;
ALTER TABLE release_bundles DROP CONSTRAINT release_bundles_locale_id_fkey;
ALTER TABLE release_bundles
    ADD CONSTRAINT release_bundles_release_fkey
        FOREIGN KEY (project_id, release_id) REFERENCES releases (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT release_bundles_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE assets ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE assets SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE assets
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT assets_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT assets_project_id_unique UNIQUE (project_id, id);

ALTER TABLE assets DROP CONSTRAINT assets_sha256_unique;
ALTER TABLE assets DROP CONSTRAINT assets_s3_key_unique;
ALTER TABLE assets
    ADD CONSTRAINT assets_project_sha256_unique UNIQUE (project_id, sha256),
    ADD CONSTRAINT assets_project_s3_key_unique UNIQUE (project_id, s3_key);

ALTER TABLE key_assets ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE key_assets SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE key_assets
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE key_assets DROP CONSTRAINT key_assets_key_id_fkey;
ALTER TABLE key_assets DROP CONSTRAINT key_assets_asset_id_fkey;
ALTER TABLE key_assets
    ADD CONSTRAINT key_assets_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT key_assets_asset_fkey
        FOREIGN KEY (project_id, asset_id) REFERENCES assets (project_id, id) ON DELETE CASCADE;
```

Create `.db/U1.12__scope_releases_assets.sql` in the same shape as U1.10 and U1.11. Note in its header that restoring the global `assets_sha256_unique` will fail outright once two projects hold the same image, which is the intended loud failure rather than a silent merge of two projects' assets.

- [ ] **Step 2: Write the failing test**

Create `integration-tests/scope_releases_test.go` (package `integrationtests`, importing `testing`, `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require`). Tasks 3, 4 and 5 run in parallel, so each owns its own test file — appending to a shared one would collide:

```go
// TestReleaseVersionsRestartPerProject: YouBiz's first release is 1.
func TestReleaseVersionsRestartPerProject(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('relscope', 'Release Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	_, err := testDB.Exec(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES (1, 9001, 'publish', 'test@you.co')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM releases WHERE version = 9001`)
	})

	_, err = testDB.Exec(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES ($1, 9001, 'publish', 'test@you.co')`, otherProject)
	require.NoError(t, err, "the same version number in another project must be allowed")

	_, err = testDB.Exec(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES (1, 9001, 'publish', 'test@you.co')`)
	requireRejected(t, err, "duplicate version within one project")
}
```

Confirm the `releases` column list against `.db/V1.03__releases_and_bundles.sql` before running and adjust the INSERTs if `created_by` differs or other columns are mandatory.

- [ ] **Step 3: Run to verify it fails, then apply and re-run**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestReleaseVersionsRestart' -v`
Expected: FAIL — no `project_id` on `releases`.

Run: `make db-test && REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestReleaseVersionsRestart' -v`
Expected: PASS.

- [ ] **Step 4: Verify the whole suite**

Run: `gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./...`
Expected: PASS, including the OTA tests — the servable index changed shape but the query still matches it.

- [ ] **Step 5: Commit**

```bash
git add .db/V1.12__scope_releases_assets.sql .db/U1.12__scope_releases_assets.sql integration-tests/scope_constraints_test.go
git commit -m "feat(db): scope releases, bundles and assets to a project"
```

---

### Task 5: Scope identity and the operational tables

**Files:**
- Create: `.db/V1.13__scope_identity.sql`, `.db/U1.13__scope_identity.sql`
- Create: `integration-tests/scope_identity_test.go`

**Interfaces:**
- Consumes: `projects` (Task 1), `keys (project_id, id)` and `locales (project_id, id)` (Task 2).
- Produces: table `user_project_roles (email, project_id, role, granted_by, granted_at)`; `users.is_platform_admin BOOLEAN NOT NULL DEFAULT false`; `project_id` on `api_tokens`, `project_settings`, `audit_events`, `import_runs`, `translation_history`, `key_history`.

- [ ] **Step 1: Write the migration**

Create `.db/V1.13__scope_identity.sql`:

```sql
-- Split identity from authorization, and scope the operational tables.
--
-- `users` keeps email and status: one person, one row, one identity. What
-- they may DO becomes per project, because a person may approve YouBiz copy
-- and have no business reading YouTrip's. The ordered-role trick survives
-- intact — viewer < editor < approver < admin still compares as one
-- inequality, it is simply read from this table now.
--
-- users.role is deliberately NOT dropped here. The middleware still reads it
-- until Plan 2 switches to per-project lookups; dropping it now would break
-- every authenticated request. Plan 2 drops it once nothing reads it.
--
-- One global privilege exists: a platform admin creates projects and grants
-- the first role in each. Without it the grant flow deadlocks on itself.
CREATE TABLE IF NOT EXISTS user_project_roles (
    email      CITEXT   NOT NULL REFERENCES users (email) ON DELETE CASCADE,
    project_id SMALLINT NOT NULL REFERENCES projects (id),
    role       TEXT     NOT NULL,
    granted_by CITEXT   NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_project_roles_pkey PRIMARY KEY (email, project_id),
    CONSTRAINT user_project_roles_role_check
        CHECK (role IN ('viewer', 'editor', 'approver', 'admin'))
);

CREATE INDEX IF NOT EXISTS idx_user_project_roles_project
    ON user_project_roles (project_id, role);

ALTER TABLE users ADD COLUMN IF NOT EXISTS is_platform_admin BOOLEAN NOT NULL DEFAULT false;

-- Every existing operator keeps exactly the access they had, on YouTrip.
INSERT INTO user_project_roles (email, project_id, role, granted_by)
SELECT email, 1, role, 'migration:V1.13' FROM users
ON CONFLICT (email, project_id) DO NOTHING;

-- Existing admins become platform admins: they are the people who must be
-- able to create the second project.
UPDATE users SET is_platform_admin = true WHERE role = 'admin';

-- A token belongs to one project, so YouBiz's CI cannot pull YouTrip's export.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE api_tokens SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE api_tokens
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT api_tokens_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

-- project_settings was named for exactly this and only ever held one project's.
ALTER TABLE project_settings ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE project_settings SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE project_settings
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT project_settings_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);
ALTER TABLE project_settings DROP CONSTRAINT project_settings_pkey;
ALTER TABLE project_settings ADD CONSTRAINT project_settings_pkey PRIMARY KEY (project_id, key);

ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE audit_events SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE audit_events
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT audit_events_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

ALTER TABLE import_runs ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE import_runs SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE import_runs
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT import_runs_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

-- History keeps the V1.08 NO ACTION semantics: an audit record outlives what
-- it describes, so these composite keys must not cascade.
ALTER TABLE translation_history ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE translation_history SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE translation_history
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;
ALTER TABLE translation_history DROP CONSTRAINT translation_history_key_id_fkey;
ALTER TABLE translation_history DROP CONSTRAINT translation_history_locale_id_fkey;
ALTER TABLE translation_history
    ADD CONSTRAINT translation_history_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id),
    ADD CONSTRAINT translation_history_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE key_history ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE key_history SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE key_history
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;
ALTER TABLE key_history DROP CONSTRAINT key_history_key_id_fkey;
ALTER TABLE key_history
    ADD CONSTRAINT key_history_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id);
```

Verify the `translation_history` locale foreign key exists under that name in Step 1's query style before relying on the DROP; V1.01 may not have declared one.

Create `.db/U1.13__scope_identity.sql`: drop `user_project_roles`, drop `is_platform_admin`, restore the single-column history keys with `NO ACTION`, restore the `project_settings` primary key to `(key)`, and drop each `project_id`. Note in the header that dropping `user_project_roles` discards every grant made after the migration ran — the roles cannot be reconstructed from `users.role`, which by then is stale.

- [ ] **Step 2: Write the failing test**

Create `integration-tests/scope_identity_test.go` (package `integrationtests`, importing `testing`, `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require`). Tasks 3, 4 and 5 run in parallel, so each owns its own test file — appending to a shared one would collide:

```go
// TestExistingOperatorsKeepTheirAccessOnYouTrip proves the V1.13 backfill
// moved every role across rather than silently dropping people.
func TestExistingOperatorsKeepTheirAccessOnYouTrip(t *testing.T) {
	var users, grants int
	require.NoError(t, testDB.QueryRow(`SELECT count(*) FROM users`).Scan(&users))
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM user_project_roles WHERE project_id = 1`).Scan(&grants))
	require.Equal(t, users, grants,
		"every user must hold a YouTrip grant after the V1.13 backfill")

	var mismatched int
	require.NoError(t, testDB.QueryRow(`
		SELECT count(*) FROM users u
		  JOIN user_project_roles r ON r.email = u.email AND r.project_id = 1
		 WHERE r.role <> u.role`).Scan(&mismatched))
	assert.Zero(t, mismatched, "the grant must carry the role the user already had")
}

// TestRoleGrantsRequireAKnownRole: the ordered-role vocabulary is closed.
func TestRoleGrantsRequireAKnownRole(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO users (email, role) VALUES ('scope-role@you.co', 'viewer')
		 ON CONFLICT (email) DO NOTHING`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM users WHERE email = 'scope-role@you.co'`)
	})

	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-role@you.co', 1, 'superuser', 'test@you.co')`)
	requireRejected(t, err, "unknown role")

	_, err = testDB.Exec(
		`INSERT INTO user_project_roles (email, project_id, role, granted_by)
		 VALUES ('scope-role@you.co', 999, 'viewer', 'test@you.co')`)
	requireRejected(t, err, "grant against a project that does not exist")
}
```

- [ ] **Step 3: Run to verify failure, then apply and re-run**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestExistingOperators|TestRoleGrants' -v`
Expected: FAIL — `relation "user_project_roles" does not exist`.

Run: `make db-test && REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestExistingOperators|TestRoleGrants' -v`
Expected: PASS.

- [ ] **Step 4: Verify the whole suite**

Run: `gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./...`
Expected: PASS. Authentication still works because `users.role` is untouched.

- [ ] **Step 5: Commit**

```bash
git add .db/V1.13__scope_identity.sql .db/U1.13__scope_identity.sql integration-tests/scope_constraints_test.go
git commit -m "feat(db): split identity from per-project authorization"
```

---

### Task 6: Project management service, admin API and CLI bootstrap

**Files:**
- Create: `pkg/service/projectsvc/projectsvc.go`, `route/project.go`, `route/project_test.go`
- Modify: `route/route.go`, `inject_service.go`, `main.go`

**Interfaces:**
- Consumes: `repository.ProjectRepository` (Task 1); `users.is_platform_admin` (Task 5).
- Produces: `projectsvc.Service` with `List(ctx, includeArchived bool) ([]model.Project, error)`, `Create(ctx, actor string, in projectsvc.NewProject) (model.Project, error)`, `Update(ctx, code string, in projectsvc.ProjectPatch) (model.Project, error)`; sentinels `projectsvc.ErrBadRequest`, `projectsvc.ErrNotFound`; handlers `Handler.ListProjects`, `Handler.CreateProject`, `Handler.PatchProject`; CLI commands `project create`, `project list`, and the `--platform-admin` flag on `user grant`.

- [ ] **Step 1: Write the failing handler test**

Create `route/project_test.go`. This mirrors `route/user_test.go` exactly: a table over the sentinel-to-status mapping driven through the handler's error mapper, plus a router test for the privilege boundary.

```go
package route

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
)

// TestProjectErrorStatusCodes pins the sentinel-to-status mapping. A
// duplicate code is the caller's mistake and must not reach 500.
func TestProjectErrorStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"bad code", fmt.Errorf("%w: code must be", projectsvc.ErrBadRequest), http.StatusBadRequest, "bad_request"},
		{"duplicate code", fmt.Errorf("create: %w", repository.ErrProjectCodeTaken), http.StatusConflict, "project_code_taken"},
		{"no such project", fmt.Errorf("project %q: %w", "nope", repository.ErrNotFound), http.StatusNotFound, "not_found"},
		{"database down", errors.New("dial tcp: connection refused"), http.StatusInternalServerError, "internal_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/projects", nil)

			(&Handler{}).projectError(w, r, "create project", tc.err)

			assert.Equal(t, tc.want, w.Code)
			assert.Contains(t, w.Body.String(), tc.code)
		})
	}
}

// TestCreateProjectRequiresPlatformAdmin: minting a project is the one
// global privilege. An admin on YouTrip is still only an admin on YouTrip,
// and must not be able to create YouBiz.
//
// The project service is nil, so a request that got past the middleware
// would panic rather than quietly pass.
func TestCreateProjectRequiresPlatformAdmin(t *testing.T) {
	router := portalRouterPlatformAdmin(t, "a@you.co", repository.RoleAdmin, false)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects",
		strings.NewReader(`{"code":"youbiz","name":"YouBiz"}`))
	r.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "forbidden")
}

// TestProjectRoutesRejectUnknownQueryParameters: these routes take no query
// parameters, and a stray one is a caller mistake to report, not ignore.
func TestProjectRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouterPlatformAdmin(t, "a@you.co", repository.RoleAdmin, true)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects?typo=1",
		strings.NewReader(`{"code":"youbiz","name":"YouBiz"}`))
	r.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "typo")
}
```

`portalRouterPlatformAdmin(t *testing.T, email, role string, isPlatformAdmin bool) http.Handler` does not exist yet — Step 4 adds it beside the existing `portalRouter` helper, as a variant that sets the platform-admin flag on the identity it injects. Read `portalRouter` in `route/user_test.go` first and extend it rather than writing a second, divergent harness.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./route/ -run 'TestCreateProject' -v`
Expected: FAIL — the handlers do not exist.

- [ ] **Step 3: Implement the service**

Create `pkg/service/projectsvc/projectsvc.go`. Mirror the shape of an existing small service — read `pkg/service/tagsvc/tagsvc.go` first for the house pattern of sentinels, validation and `WithTransaction` usage.

The rules the service owns:

```go
// codePattern constrains what may appear in a URL path. The database has the
// same CHECK; this exists so a caller gets 400 with a message instead of a
// driver error.
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Create validates, then writes the project and its creator's admin grant in
// ONE transaction. A project whose creator cannot administer it is a project
// nobody can configure, and the two writes must not be able to diverge.
```

`Create` takes `NewProject{Code, Name, LokaliseProjectID string}`, validates the code against `codePattern` (else `ErrBadRequest`), and inside `WithTransaction` calls `projects.Create` then inserts the actor's `admin` grant. `ErrProjectCodeTaken` passes through untouched for the handler to map. `Update` takes `ProjectPatch{Name, Status, LokaliseProjectID string}` and rejects a status outside `active`/`archived`.

- [ ] **Step 4: Implement the handlers and routes**

Create `route/project.go` with `ListProjects`, `CreateProject` and `PatchProject`. Each begins with `rejectUnknownParams(r)` — the convention is that a client who misspells a parameter is told. Map errors through the existing `portalError` mapper, adding one case: `repository.ErrProjectCodeTaken` to 409 with code `project_code_taken`.

In `route/route.go`, register inside the `/api/v1` group, in a new platform-admin group beside the existing admin group:

```go
r.Group(func(r chi.Router) {
    r.Use(handler.requirePlatformAdmin)
    r.Post("/projects", handler.CreateProject)
    r.Patch("/projects/{project}", handler.PatchProject)
})
```

`GET /projects` goes in the authenticated-any-role group, since every operator needs to know which projects they can reach. Write `requirePlatformAdmin` beside the existing role middleware in the same file, reading the `is_platform_admin` column loaded with the user, and answering 403 with code `forbidden` when it is false.

Also add the test helper Step 1 depends on, beside the existing `portalRouter` in `route/user_test.go`:

```go
// portalRouterPlatformAdmin is portalRouter with control over the global
// privilege. Project creation is the one thing a project admin may not do,
// so the flag has to be settable independently of the role.
func portalRouterPlatformAdmin(t *testing.T, email, role string, isPlatformAdmin bool) http.Handler {
	t.Helper()
	// Build exactly as portalRouter does, then set is_platform_admin on the
	// identity the stub authenticator injects.
}
```

Fill the body by extending `portalRouter`'s construction — read it first and factor the shared part rather than copying it, so the two helpers cannot drift.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./route/ -run 'TestCreateProject|TestListProjects' -v`
Expected: PASS.

- [ ] **Step 6: Add the CLI bootstrap commands**

In `main.go`, add a `project` command with `create` and `list` subcommands mirroring the structure of the existing `user` command — read `userCommand` first and follow it exactly. `project create` takes `--code`, `--name`, `--lokalise-project-id` and `--actor`; `project list` takes `--all` to include archived.

Add `--platform-admin` to `user grant`. Without it there is no way to create the first platform admin, and the API cannot bootstrap itself.

- [ ] **Step 7: Regenerate wire and verify everything**

Run: `make gen-wire && gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add pkg/service/projectsvc route/project.go route/project_test.go route/route.go inject_service.go main.go wire_gen.go
git commit -m "feat(projects): manage projects through the API and CLI"
```

---

### Task 7: Runtime locale management

**Files:**
- Modify: `pkg/repository/locale.go`, `pkg/service/projectsvc/projectsvc.go`, `route/project.go`, `route/route.go`, `integration-tests/project_test.go`

**Interfaces:**
- Consumes: `projectsvc.Service` (Task 6); `locales.status` and the per-project uniqueness constraints (Task 2).
- Produces: `LocaleRepository.Create(ctx, tx, l model.Locale) (model.Locale, error)` and `Update(ctx, tx, projectID int16, code string, l model.Locale) (model.Locale, error)`; `projectsvc.AddLocale` and `projectsvc.UpdateLocale`; handlers `Handler.AddLocale`, `Handler.PatchLocale`.

- [ ] **Step 1: Write the failing test**

Append to `integration-tests/project_test.go`:

```go
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
		_, _ = testDB.Exec(`DELETE FROM locales WHERE id = $1`, created.ID)
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
		_, _ = testDB.Exec(`DELETE FROM locales WHERE id = $1`, created.ID)
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
```

- [ ] **Step 2: Run to verify failure**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestAddingALocale|TestArchivedLocales' -v`
Expected: FAIL — `repo.Create undefined`.

- [ ] **Step 3: Implement the repository writes**

In `pkg/repository/locale.go`, add these two methods to the interface and implement them:

```go
// ErrLocaleCodeTaken is returned when a project already has that locale.
// Exported so the handler answers 409 rather than letting a unique violation
// surface as a 500.
var ErrLocaleCodeTaken = errors.New("locale code already in use for this project")

func (r *localeRepository) Create(ctx context.Context, tx *gorm.DB, l model.Locale) (model.Locale, error) {
	status := l.Status
	if status == "" {
		status = "active"
	}
	row := r.db(ctx, tx).Raw(
		`INSERT INTO locales
		     (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, code) DO NOTHING
		 RETURNING `+localeColumns,
		l.ProjectID, l.Code, l.FlutterDir, l.AndroidValuesDir,
		l.IOSLproj, l.SortOrder, status).Row()

	var out model.Locale
	err := row.Scan(&out.ID, &out.ProjectID, &out.Code, &out.FlutterDir,
		&out.AndroidValuesDir, &out.IOSLproj, &out.SortOrder, &out.Status)
	if isNoRows(err) {
		return model.Locale{}, ErrLocaleCodeTaken
	}
	if err != nil {
		return model.Locale{}, fmt.Errorf("create locale: %w", err)
	}
	return out, nil
}

func (r *localeRepository) Update(ctx context.Context, tx *gorm.DB, projectID int16, code string, l model.Locale) (model.Locale, error) {
	row := r.db(ctx, tx).Raw(
		`UPDATE locales
		    SET flutter_dir = ?, android_values_dir = ?, ios_lproj = ?,
		        sort_order = ?, status = ?
		  WHERE project_id = ? AND code = ?
		 RETURNING `+localeColumns,
		l.FlutterDir, l.AndroidValuesDir, l.IOSLproj,
		l.SortOrder, l.Status, projectID, code).Row()

	var out model.Locale
	err := row.Scan(&out.ID, &out.ProjectID, &out.Code, &out.FlutterDir,
		&out.AndroidValuesDir, &out.IOSLproj, &out.SortOrder, &out.Status)
	if isNoRows(err) {
		return model.Locale{}, ErrNotFound
	}
	if err != nil {
		return model.Locale{}, fmt.Errorf("update locale: %w", err)
	}
	return out, nil
}
```

The `code` is deliberately not updatable: it is the identifier callers address the locale by, and renaming it would silently orphan every reference. Add `"errors"` to the file's imports.

Do not add a delete method. Translations and history reference locales, and V1.08 settled that an audit trail outlives its subject.

- [ ] **Step 4: Implement the service rules and handlers**

In `projectsvc`, add `AddLocale` and `UpdateLocale`. They validate:

- the code matches `^[a-z]{2}(-[A-Z]{2})?$`, else `ErrBadRequest`;
- all three export directory names are non-empty, else `ErrBadRequest`;
- status is `active` or `archived`.

A duplicate export directory is left to the database, whose unique constraint refuses it — the handler maps the resulting conflict to 409 with `locale_directory_taken`. Add `AddLocale`/`PatchLocale` handlers to `route/project.go`, registered under the platform-admin group at `POST /projects/{project}/locales` and `PATCH /projects/{project}/locales/{code}`, each beginning with `rejectUnknownParams(r)`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `REQUIRE_TEST_DB=1 go test -race ./integration-tests/ -run 'TestAddingALocale|TestArchivedLocales' -v && go test -race ./route/ -v`
Expected: PASS.

- [ ] **Step 6: Update the documentation**

The docs describe single-project behaviour that this plan changes. Update:

- `docs/DATA_MODEL.md` — add `projects` and `user_project_roles` to the entity reference, and note that locale rows are per project and admin-managed.
- `docs/API.md` — add the project and locale admin endpoints with their roles and error codes.
- `docs/OPERATIONS.md` — add the `project create`, `project list` and `user grant --platform-admin` commands.
- `CLAUDE.md` — the "What This Is" section says u-l10n serves the YouTrip mobile apps; it serves any number of projects now. `pkg/repository/locale.go`'s old claim that locales are not writable at runtime is already fixed in Task 2.

- [ ] **Step 7: Full verification**

Run: `gofmt -w . && go build ./... && go vet ./... && REQUIRE_TEST_DB=1 go test -race ./...`
Expected: PASS across every package.

- [ ] **Step 8: Commit**

```bash
git add pkg/repository/locale.go pkg/service/projectsvc route/project.go route/route.go integration-tests/project_test.go docs CLAUDE.md
git commit -m "feat(locales): manage locales per project at runtime"
```

---

## What Plan 2 inherits

Stated here so the next plan starts from facts rather than archaeology:

1. **Temporary column defaults.** Every `project_id` added in Tasks 2–5 carries `DEFAULT 1`. Plan 2's final migration drops all of them, and its acceptance test asserts that no `project_id` column has a default — otherwise a forgotten scope silently becomes YouTrip.
2. **Hardcoded scope call sites.** Task 2 Step 7 leaves `TODO(plan-2)` markers wherever `1` is passed as the project. Plan 2 begins by grepping for that exact string; when the grep is empty and the tests pass, the sweep is done.
3. **`users.role` is still authoritative.** V1.13 backfilled `user_project_roles` but did not drop `users.role`, because the middleware still reads it. Plan 2 switches the lookup, then drops the column.
4. **Asset S3 keys have no project prefix yet.** V1.12 scoped the table; `assetsvc` still builds keys without the project segment. Plan 2 changes the key builder — existing rows keep their old keys, which stay valid because the uniqueness constraint is now per project.
5. **The advisory lock is still global.** `mergesvc` uses the one-argument `pg_advisory_xact_lock(8675309)`. Plan 2 moves it to the two-argument form with `project_id`, and adds the inverted concurrency test proving two projects merge concurrently.
