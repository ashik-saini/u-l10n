-- Operational tables: audit trail, import bookkeeping, project settings.

-- ---------------------------------------------------------------------------
-- audit_events
--
-- Broader than key/translation history: this records actions rather than value
-- changes. Admin direct-edits to master, role grants, token creation, asset
-- views and merges all land here.
--
-- Asset views are audited deliberately — the images contain customer PII, so
-- "who looked at this" is a question that must be answerable.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_events (
    id          BIGSERIAL PRIMARY KEY,
    actor       CITEXT NOT NULL,
    action      TEXT   NOT NULL,
    -- Free-form target reference, e.g. 'key:1234', 'branch:copy-fixes',
    -- 'asset:88'. Deliberately not a foreign key: an audit record must outlive
    -- whatever it describes.
    target      TEXT   NOT NULL DEFAULT '',
    metadata    JSONB  NOT NULL DEFAULT '{}'::JSONB,
    request_id  TEXT   NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor      ON audit_events (actor, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_target     ON audit_events (target, created_at DESC);

-- ---------------------------------------------------------------------------
-- import_runs
--
-- One row per Lokalise import attempt, including dry runs. The importer is
-- idempotent and resumable, so a failed run is expected to be re-run rather
-- than cleaned up by hand; this table is how you tell what it did.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS import_runs (
    id                 BIGSERIAL PRIMARY KEY,
    status             TEXT   NOT NULL DEFAULT 'running',
    dry_run            BOOLEAN NOT NULL DEFAULT FALSE,
    keys_created       INT    NOT NULL DEFAULT 0,
    keys_updated       INT    NOT NULL DEFAULT 0,
    translations_upserted INT NOT NULL DEFAULT 0,
    -- Non-fatal observations: unknown platform, name collision survived by an
    -- override, value present in the API but absent from the file export.
    warnings           JSONB  NOT NULL DEFAULT '[]'::JSONB,
    error              TEXT   NOT NULL DEFAULT '',
    started_by         CITEXT NOT NULL,
    started_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at        TIMESTAMPTZ,
    CONSTRAINT import_runs_status_check
        CHECK (status IN ('running', 'succeeded', 'failed', 'rolled_back')),
    CONSTRAINT import_runs_counts_check CHECK (
        keys_created >= 0 AND keys_updated >= 0 AND translations_upserted >= 0
    )
);

CREATE INDEX IF NOT EXISTS idx_import_runs_started_at ON import_runs (started_at DESC);

-- ---------------------------------------------------------------------------
-- project_settings
--
-- Single-row key/value configuration that must be editable at runtime by an
-- admin, as opposed to the environment-variable configuration that requires a
-- deploy. Kept minimal on purpose.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS project_settings (
    key        TEXT   NOT NULL,
    value      JSONB  NOT NULL,
    updated_by CITEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT project_settings_pkey PRIMARY KEY (key)
);
