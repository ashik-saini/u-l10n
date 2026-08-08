-- Copy-on-write branching and the merge-request workflow.
--
-- A branch does NOT copy master's ~36,000 values. It stores only deltas, and a
-- read resolves as: delta row if present, otherwise the master row.

CREATE TABLE IF NOT EXISTS branches (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT   NOT NULL,
    description    TEXT   NOT NULL DEFAULT '',
    status         TEXT   NOT NULL DEFAULT 'open',
    created_by     CITEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Any write to the branch bumps this. The merge transaction compares it
    -- against merge_requests.approved_at to detect an approval that has been
    -- invalidated by later edits — you cannot get a diff approved and then
    -- quietly change it.
    last_edited_at TIMESTAMPTZ,
    merged_at      TIMESTAMPTZ,
    CONSTRAINT branches_name_unique UNIQUE (name),
    CONSTRAINT branches_status_check CHECK (status IN ('open', 'merged', 'closed'))
);

-- ---------------------------------------------------------------------------
-- branch_translations — copy-on-write value deltas
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS branch_translations (
    branch_id           BIGINT   NOT NULL REFERENCES branches (id) ON DELETE CASCADE,
    key_id              BIGINT   NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    locale_id           SMALLINT NOT NULL REFERENCES locales (id),
    -- NULL only when is_removed is true.
    value               TEXT,
    render_hint         TEXT     NOT NULL DEFAULT 'plain',
    -- Tombstone: this branch removes the translation that exists on master.
    -- Distinct from value = '' (explicitly empty) and from having no delta row
    -- at all (branch does not touch this pair).
    is_removed          BOOLEAN  NOT NULL DEFAULT FALSE,
    -- What master's version was when this branch FIRST touched this pair.
    -- Captured on insert and never updated by later edits on the same branch:
    -- it records the starting point, not the work.
    --
    -- 0 means no master row existed. At merge, the entire value-conflict rule
    -- is one comparison:
    --   COALESCE(master.version, 0) <> base_master_version  ->  CONFLICT
    -- which covers edit/edit, create/create, remove/edit and edit/delete races.
    base_master_version INT      NOT NULL,
    updated_by          CITEXT   NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT branch_translations_pkey PRIMARY KEY (branch_id, key_id, locale_id),
    CONSTRAINT branch_translations_render_hint_check
        CHECK (render_hint IN ('plain', 'cdata')),
    CONSTRAINT branch_translations_base_version_check CHECK (base_master_version >= 0),
    -- A removal carries no value; a non-removal must carry one (possibly '').
    CONSTRAINT branch_translations_removed_value_check CHECK (
        (is_removed AND value IS NULL) OR (NOT is_removed AND value IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_branch_translations_key_locale
    ON branch_translations (key_id, locale_id);

-- ---------------------------------------------------------------------------
-- branch_keys — copy-on-write metadata deltas
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS branch_keys (
    branch_id           BIGINT NOT NULL REFERENCES branches (id) ON DELETE CASCADE,
    -- NULL for a key created on this branch that does not yet exist on master.
    key_id              BIGINT REFERENCES keys (id) ON DELETE CASCADE,
    name                TEXT   NOT NULL,
    description         TEXT   NOT NULL DEFAULT '',
    platforms           TEXT[] NOT NULL,
    android_name        TEXT,
    ios_name            TEXT,
    status              TEXT   NOT NULL DEFAULT 'active',
    -- Same semantics as branch_translations.base_master_version, anchored on
    -- keys.version.
    base_master_version INT    NOT NULL,
    updated_by          CITEXT NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT branch_keys_status_check CHECK (status IN ('active', 'deleted', 'draft')),
    CONSTRAINT branch_keys_base_version_check CHECK (base_master_version >= 0),
    CONSTRAINT branch_keys_platforms_check CHECK (
        platforms <@ ARRAY['flutter', 'android', 'ios']::TEXT[]
        AND cardinality(platforms) > 0
    )
);

-- One delta per existing key per branch. Partial, because key_id is NULL for
-- keys created on the branch and NULLs do not collide in a plain unique index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_branch_keys_branch_key
    ON branch_keys (branch_id, key_id) WHERE key_id IS NOT NULL;

-- A branch may not introduce the same new name twice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_branch_keys_branch_name
    ON branch_keys (branch_id, name);

-- ---------------------------------------------------------------------------
-- merge_requests
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS merge_requests (
    id          BIGSERIAL PRIMARY KEY,
    branch_id   BIGINT NOT NULL REFERENCES branches (id) ON DELETE CASCADE,
    title       TEXT   NOT NULL,
    description TEXT   NOT NULL DEFAULT '',
    status      TEXT   NOT NULL DEFAULT 'open',
    created_by  CITEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by CITEXT,
    -- Compared against branches.last_edited_at inside the merge transaction.
    -- A later edit re-opens the request, so this cannot go stale silently.
    approved_at TIMESTAMPTZ,
    merged_at   TIMESTAMPTZ,
    CONSTRAINT merge_requests_status_check
        CHECK (status IN ('open', 'approved', 'changes_requested', 'rejected', 'merged', 'closed'))
);

-- At most one LIVE merge request per branch. Terminal states are excluded so a
-- branch can be reopened with a fresh request after a rejection.
CREATE UNIQUE INDEX IF NOT EXISTS idx_merge_requests_one_live_per_branch
    ON merge_requests (branch_id)
    WHERE status NOT IN ('merged', 'closed', 'rejected');

CREATE TABLE IF NOT EXISTS merge_request_events (
    id               BIGSERIAL PRIMARY KEY,
    merge_request_id BIGINT NOT NULL REFERENCES merge_requests (id) ON DELETE CASCADE,
    event            TEXT   NOT NULL,
    comment          TEXT   NOT NULL DEFAULT '',
    -- 'system' for automatic transitions, such as an approval invalidated by a
    -- subsequent branch edit.
    actor            CITEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT merge_request_events_event_check CHECK (event IN (
        'created', 'approved', 'changes_requested', 'rejected',
        'reopened', 'closed', 'merged', 'approval_invalidated'
    ))
);

CREATE INDEX IF NOT EXISTS idx_merge_request_events_mr
    ON merge_request_events (merge_request_id, created_at);

-- Stored human decisions for conflicts. The merge transaction refuses to
-- proceed while any detected conflict lacks a resolution: auto-resolving means
-- silently choosing one person's words over another's.
CREATE TABLE IF NOT EXISTS merge_conflict_resolutions (
    merge_request_id BIGINT   NOT NULL REFERENCES merge_requests (id) ON DELETE CASCADE,
    key_id           BIGINT   NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    -- NULL for a key-metadata conflict, which has no locale dimension.
    locale_id        SMALLINT REFERENCES locales (id),
    resolution       TEXT     NOT NULL,
    resolved_by      CITEXT   NOT NULL,
    resolved_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT merge_conflict_resolutions_resolution_check
        CHECK (resolution IN ('mine', 'master'))
);

-- COALESCE because NULL locale_id (metadata conflicts) would not otherwise
-- collide, allowing duplicate resolutions for the same key.
CREATE UNIQUE INDEX IF NOT EXISTS idx_merge_conflict_resolutions_unique
    ON merge_conflict_resolutions (merge_request_id, key_id, COALESCE(locale_id, -1));
