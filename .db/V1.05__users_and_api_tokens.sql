-- Identity and authorization.
--
-- u-l10n owns its own role model, keyed on email. It is NOT derived from the
-- portal's yp_* Google Workspace groups: a designer may be an l10n editor and
-- nothing else, and overloading another system's authorization model means
-- inheriting its every future change.

CREATE TABLE IF NOT EXISTS users (
    id         BIGSERIAL PRIMARY KEY,
    -- CITEXT, so Ashik.Saini@you.co and ashik.saini@you.co are the same person
    -- by type rather than by remembering LOWER() at every call site.
    email      CITEXT NOT NULL,
    -- Roles are ORDERED: viewer < editor < approver < admin. Ordering lets
    -- middleware express "editor or above" as one comparison instead of set
    -- membership repeated at every handler.
    role       TEXT   NOT NULL DEFAULT 'viewer',
    status     TEXT   NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_email_unique UNIQUE (email),
    CONSTRAINT users_role_check CHECK (role IN ('viewer', 'editor', 'approver', 'admin')),
    CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled'))
);

-- ---------------------------------------------------------------------------
-- api_tokens
--
-- For scripts and CI (the u-mobile export pull), not for humans.
--
-- Only the SHA-256 of the token is stored. A database dump therefore hands
-- over no working credentials — the same reasoning as password hashing. The
-- plaintext prefix exists so the UI can render a recognisable "ul10n_a3f9..."
-- in a list. The full token is shown exactly once, at creation.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT   NOT NULL,
    token_sha256 TEXT   NOT NULL,
    token_prefix TEXT   NOT NULL,
    scope        TEXT   NOT NULL DEFAULT 'read_export',
    created_by   CITEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    revoked_by   CITEXT,
    CONSTRAINT api_tokens_sha256_unique UNIQUE (token_sha256),
    CONSTRAINT api_tokens_sha256_format_check CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT api_tokens_scope_check CHECK (scope IN ('read_export', 'read_write')),
    CONSTRAINT api_tokens_revocation_consistency_check CHECK (
        (revoked_at IS NULL AND revoked_by IS NULL)
        OR (revoked_at IS NOT NULL AND revoked_by IS NOT NULL)
    )
);

-- Authentication looks up by hash on every scripted request; only live tokens
-- are candidates.
CREATE INDEX IF NOT EXISTS idx_api_tokens_live
    ON api_tokens (token_sha256) WHERE revoked_at IS NULL;
