# Multi-project support — design

**Date:** 2026-08-09
**Status:** approved, not yet planned
**Context:** u-l10n serves one project (YouTrip). It must serve any number — YouTrip and YouBiz today — each with its own locale set, its own team, and its own release stream.

## Requirements

Established with the requester before design:

1. **Content is fully independent.** A key named `login_title` in YouTrip has nothing to do with one in YouBiz. No shared keys, no fallback resolution, no cross-project inheritance.
2. **Locale sets differ per project from day one.** YouBiz does not ship YouTrip's six locales.
3. **Roles are per project.** A person may be an approver on YouBiz and a viewer, or nothing, on YouTrip.
4. **Each project imports from its own Lokalise project.**
5. **Extensible to any number of projects and any number of locales** — both are runtime data, not deploy artifacts.

Nothing is in production: Phase 11 (the Flutter OTA client) was never built, so no shipped client pins the OTA URL, and the only data is the seeded YouTrip corpus. The design therefore optimises for the right end state rather than for backward compatibility.

## Approach

One deployment and one database, with a project dimension in the schema.

Two alternatives were considered and rejected. **Separate deployments per project** isolate perfectly and need almost no code, but impose a permanent operational tax: every change deploys N times, users and tokens exist N times, the portal targets N backends, admins get no cross-project view, and each new brand is new infrastructure. **Postgres schema-per-project** avoids query changes but makes Flyway run per schema, turns project creation into DDL, and makes `search_path` a footgun under connection pooling — for isolation that composite foreign keys already provide.

The chosen approach's one real risk is a forgotten `WHERE project_id = …` leaking data across projects. That risk is mitigated structurally (below) and is bounded; the alternatives' costs recur forever.

## Data model

### Projects as the root

```sql
CREATE TABLE projects (
    id                  SMALLSERIAL PRIMARY KEY,
    code                TEXT NOT NULL,   -- URL segment: 'youtrip', 'youbiz'
    name                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',   -- active | archived
    lokalise_project_id TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT projects_code_unique UNIQUE (code)
);
```

`code` is validated as a URL-safe slug because it appears in every path.

### Identity-owning tables carry `project_id`

`locales`, `keys`, `tags`, `branches`, `releases`, `assets`, `api_tokens`, `project_settings`, `audit_events`, `import_runs`.

Uniqueness becomes composite:

| Table | Was | Becomes |
|---|---|---|
| `keys` | `UNIQUE (name) WHERE status='active'` | `UNIQUE (project_id, name) WHERE status='active'` |
| `keys` | `UNIQUE (lokalise_key_id)` | `UNIQUE (project_id, lokalise_key_id)` |
| `locales` | `UNIQUE (code)` | `UNIQUE (project_id, code)` |
| `tags` | `UNIQUE (name)` | `UNIQUE (project_id, name)` |
| `branches` | `UNIQUE (name)` | `UNIQUE (project_id, name)` |
| `releases` | `UNIQUE (version)` | `UNIQUE (project_id, version)` |
| `assets` | `UNIQUE (sha256)`, `UNIQUE (s3_key)` | `UNIQUE (project_id, sha256)`, `UNIQUE (project_id, s3_key)` |

Each parent also gains `UNIQUE (project_id, id)` — redundant on its own, but required as the target of the composite foreign keys below.

### Child tables inherit scope through composite foreign keys

`translations`, `branch_translations`, `branch_keys`, `key_tags`, `key_assets`, `merge_requests`, `merge_conflict_resolutions`, `release_bundles`, `translation_history`, `key_history` each carry `project_id`, and their foreign keys become composite:

```sql
FOREIGN KEY (project_id, key_id)    REFERENCES keys    (project_id, id),
FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id)
```

This is the load-bearing part of the design. A row pairing one project's key with another project's locale is rejected by PostgreSQL rather than by a reviewer noticing a missing clause — and since locale sets now differ per project, such a pairing is not hypothetical. Illegal states are unrepresentable for writes; reads are protected by the explicit-scope rule under *Implementation rules*.

`merge_requests` takes its composite key to `branches (project_id, id)`; `merge_request_events` needs no column of its own, since it references nothing but its merge request. History tables keep the V1.08 `NO ACTION` semantics: an audit record outlives what it describes.

### Assets are isolated per project

`assets` gains `project_id`, and the S3 key gains a project prefix (`projects/<code>/screenshots/<aa>/<bb>/<sha>.<ext>`), so content addressing holds within a project. Identical bytes uploaded to two projects are stored twice — negligible for screenshots, and it buys a permission model that is a column rather than a join through `key_assets`, plus isolation at the storage layer.

### Accounts split identity from authorization

`users` keeps `email` and `status` as the global identity — one person, one row. `users.role` moves out:

```sql
CREATE TABLE user_project_roles (
    email      CITEXT   NOT NULL REFERENCES users (email) ON DELETE CASCADE,
    project_id SMALLINT NOT NULL REFERENCES projects (id),
    role       TEXT     NOT NULL,   -- viewer | editor | approver | admin
    granted_by CITEXT   NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (email, project_id)
);
```

The ordered-role comparison (`viewer < editor < approver < admin`) survives unchanged; it is simply read from this table. `users` gains a platform-admin flag, the only global privilege, which implies admin on every project — otherwise creating the first project and granting the first role has no path in. `api_tokens` gains a required `project_id`.

### Projects and locales are managed at runtime

Locales are currently seeded by `INSERT` inside `V1.00`, which makes adding one a deploy. For arbitrary locale counts they become admin-managed data:

- `POST /api/v1/projects`, `PATCH /api/v1/projects/{project}` — platform admin.
- `POST /api/v1/projects/{project}/locales`, `PATCH .../locales/{code}` — code, the three export directory names, sort order, status.
- **Archive, never delete.** Translations and history reference locales. Archived locales drop out of exports and OTA; their rows remain.

Adding a locale to a project with thousands of keys writes no translation rows and needs no backfill: absent means untranslated, which means omitted from exports. The three-state rule makes this free.

New constraint: `UNIQUE (project_id, flutter_dir)`, and likewise for `android_values_dir` and `ios_lproj`. Without it an admin could aim two locales at `values/` and the export zip would silently write one over the other.

## API surface

Portal routes move under `/api/v1/projects/{project}/…` — keys, translations, branches, merge-requests, tags, assets, releases, export.

Global routes, because they concern the person rather than the content:

- `GET /api/v1/me` — now returns the caller's projects and role in each, so the portal can render a project switcher.
- `GET /api/v1/projects` — what the caller can reach.
- Platform-admin routes for project creation and role grants.
- `/healthz`, `/readyz` — unchanged.

OTA becomes `GET /ota/v1/{project}/bundles/{locale}`, public and rate-limited as today. ETag/304, the kill switch, and the `min_app_version` floor are unchanged in behaviour; the floor is now per project, so each app has an independent version line.

### Authorization and error semantics

Middleware verifies the Google token, loads the user, resolves `{project}`, and looks up that user's role in that project. Tokens are refused against a project other than their own.

| Case | Answer |
|---|---|
| Unknown project code | 404 |
| Known project, caller holds no role | 403 — matching the existing "unknown email is 403, not 401" decision |
| Resource id belongs to another project | **404, not 403** |

The last rule matters: a 403 would confirm the resource exists and leak across the boundary the roles exist to protect.

## Workflow scoping

- **Branches** are unique per project, so both teams may run a `q3-copy`.
- **Releases** are monotonic per project; YouBiz starts at 1.
- **The merge transaction is logically unchanged** — same seven steps, same row locks, same version-predicated applies, same `ErrConcurrentMasterWrite`. Every statement gains its project scope.
- **The advisory lock moves to the two-argument form**, `pg_advisory_xact_lock(8675309, project_id)`: merges serialize within a project and run concurrently across projects. Today's single global constant would queue YouBiz behind YouTrip for no reason.

## Tooling

Every CLI command takes `--project`. `import` reads its Lokalise project id from the project row rather than a flag. Two bootstrap commands are added — create a project, and grant platform admin — since otherwise the first project and the first grant are unreachable.

## Migrations

`V1.09`–`V1.13`, grouped by domain, forward-only:

| Migration | Contents |
|---|---|
| `V1.09` | `projects` table; seed `youtrip` |
| `V1.10` | Core: `locales`, `keys`, `translations`, `tags`, `key_tags` |
| `V1.11` | Workflow: `branches`, `branch_*`, `merge_*` |
| `V1.12` | `releases`, `release_bundles`, `assets`, `key_assets` |
| `V1.13` | Identity: `user_project_roles`, `api_tokens`, `project_settings`, `audit_events`, `import_runs`, history tables |

Each adds the column nullable, backfills existing rows to `youtrip`, then sets `NOT NULL` and swaps the constraints. `V1.13` moves each existing user's role into a YouTrip grant. Companion `U1.NN` files are written as documentation, never executed.

Every hot-path index is rebuilt to lead with `project_id` — key browse, token auth, and in particular the OTA servable lookup, which becomes `(project_id, version DESC) WHERE rolled_back_at IS NULL`.

## Implementation rules

**Scope is an explicit parameter, never read from request context.** Repositories take `projectID` alongside the existing optional `tx`. A scope that arrives invisibly is a scope that gets forgotten, and the composite foreign keys catch only bad writes — a missing filter on a read is silent. `key.go` already records this lesson for branches: "a filter that quietly consulted master while the caller asked for a branch is the bug that reaches production." Cross-project leakage is the same bug with a worse blast radius.

Roughly 42 SQL statements across ten repository files are in scope, plus the service and handler signatures that thread the parameter through.

The format engines (`pkg/parse`, `pkg/export`) know nothing about projects and do not change.

## Testing

**An isolation suite**, seeded with *deliberately identical* names in both projects — same key names, same branch names, same release numbers. A forgotten `WHERE project_id` then surfaces as a wrong or duplicated row rather than as an empty result nobody notices.

**Cross-boundary cases written to fail**, per the repo's standing doctrine that a constraint test must prove refusal:

- A translation pairing project A's key with project B's locale is refused by the database (23503).
- A token is refused against another project's export (403).
- Another project's key id answers 404.
- Two locales in one project cannot claim the same export directory.

**An inverted concurrency test:** two projects merging simultaneously must **not** serialize. This is what proves the lock key is genuinely per-project, and it is mutation-checkable the same way the original lock test is — revert to the one-argument form and it must go red.

## Rollout

1. Schema migrations with backfill to `youtrip`.
2. Code scoping plus the isolation suite.
3. Portal project switcher against `GET /api/v1/me`.
4. Create `youbiz`, add its locales, import from its Lokalise project.

## Open questions

None blocking. Two to revisit once several projects release frequently: a retention policy for `release_bundles`, which grows as releases × active locales per project; and whether archived projects should be excluded from the OTA path entirely or answer 410 like a rolled-back release.
