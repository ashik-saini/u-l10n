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
