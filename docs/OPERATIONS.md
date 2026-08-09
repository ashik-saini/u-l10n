# u-l10n — operations runbook

How to run it, configure it, and what to do at 3am. For how the pieces fit
together see [ARCHITECTURE.md](ARCHITECTURE.md); for setup detail the
[README](../README.md) is authoritative — this file only summarizes it.

## 1. Running it

### Local dev

```bash
make run-local     # start Homebrew postgresql@14, create u_l10n_local if absent, run on :8080
make seed-local    # load the committed u-mobile tree (U_MOBILE_PATH ?= ../../FE/u-mobile)
make seed-local DRY_RUN=--dry-run   # rehearse: full pipeline in a transaction, rolled back
make s3-cred-local # write the placeholder S3 credential file storage/v4 refuses to boot without
```

`run-local` and `seed-local` both depend on `db-local` and `s3-cred-local`, so
they are self-contained. The S3 file (`.local/s3_storage.yaml`, gitignored) is
**not a credential** — storage/v4 validates its existence at startup even
though nothing local calls S3.

Verify:

```bash
curl -s localhost:8080/healthz | jq   # {"status":"ok","service":"u-l10n"}
curl -s localhost:8080/readyz  | jq   # {"status":"ready","checks":{"database":"ok"}}
```

### CLI commands

One binary, urfave/cli. With no subcommand it serves.

| Command | What it is for |
|---------|----------------|
| *(none)* / default | Run the HTTP server until SIGINT/SIGTERM, then drain |
| `seed-from-files` | Load a committed u-mobile tree. Disaster recovery, drift reconciliation at cutover |
| `import` | Import the Lokalise project: values from the API, key presence from a file export |
| `token create` / `token revoke` | Issue and revoke `X-Api-Token` credentials for scripts and CI |
| `user grant` / `user list` | Create/promote portal operators; the only way to create the first admin |

**`seed-from-files`** — flags:

```bash
u-l10n seed-from-files --root <u-mobile path> --actor you@you.co [--dry-run]
```

`--actor` is recorded as `updated_by`; writes to customer copy must be
attributable. `--dry-run` runs the real pipeline in a transaction and rolls it
back — every constraint is exercised, nothing is left behind.

**`import`** — flags:

```bash
LOKALISE_API_TOKEN=... LOKALISE_PROJECT_ID=... \
  u-l10n import --export-root <dir of Lokalise file exports> --actor you@you.co [--dry-run]
```

- `--token` / `$LOKALISE_API_TOKEN` (read scope) and `--project` /
  `$LOKALISE_PROJECT_ID` are **required**. Prefer the env vars: a token typed
  as a flag lands in shell history and `ps` output.
- `--export-root` is the presence oracle: the API alone cannot distinguish a
  deliberately-blank translation from an untranslated one.

**`token`**:

```bash
u-l10n token create --name u-mobile-ci --scope read_export --actor you@you.co
u-l10n token revoke --name u-mobile-ci --actor you@you.co
```

`--scope` is `read_export` (default) or `read_write`. The plaintext is printed
to stdout **once** and never logged; only its SHA-256 is stored.

**`user`**:

```bash
u-l10n user grant --email you@you.co --role admin --actor you@you.co
u-l10n user list
```

`--role`: `viewer` (default), `editor`, `approver`, `admin`. `--status`:
`active` (default) or `disabled`. Idempotent, and writes an `audit_events`
row. On a fresh database nobody is an admin and the API cannot create one —
this command is the bootstrap, run from the pod with its DB credentials.

## 2. Configuration

Everything arrives as environment variables (`configstruct` tags in
`pkg/config/config.go`). In Kubernetes: ConfigMap via `envFrom`. **Invalid
config fails at startup**, not at first request.

| Variable | Default | What it does | What breaks if wrong |
|----------|---------|--------------|----------------------|
| `SERVICECONFIG_ENV` | `local` | Deployment environment label (local, dev, sit, prod) | Cosmetic — logs and traces mislabel the environment |
| `SERVICECONFIG_HTTP_PORT` | `8080` | HTTP listen port | Outside 1–65535 refuses to boot; wrong value and the Service selector finds nothing |
| `SERVICECONFIG_REQUEST_TIMEOUT` | `60s` | Per-request deadline (chi Timeout middleware); propagates through context, so a timed-out request also cancels its in-flight query | `<= 0` refuses to boot. Too low kills slow-but-legitimate exports; too high lets a stuck query hold a connection for that long |
| `SERVICECONFIG_SHUTDOWN_TIMEOUT` | `15s` | Drain window after SIGTERM | `<= 0` refuses to boot. Set it **below** `terminationGracePeriodSeconds` or Kubernetes SIGKILLs mid-drain and in-flight requests are severed |
| `SERVICECONFIG_GOOGLE_OAUTH_AUDIENCE` | *(empty)* | The portal's OAuth client id. When set, access tokens issued to any other OAuth client are refused | **Fails OPEN when unset**: any Google OAuth client can mint an access token for a `you.co` user and it is accepted here as proof of intent to use u-l10n. Empty by default only because bo-api has the same gap. **Set it everywhere the portal runs.** Wrong value locks every operator out (all tokens refused) |
| `SERVICECONFIG_RATE_LIMIT_ENABLE` | `true` | Per-client-IP token bucket across `/api/v1` and `/ota/v1` | `false` removes a stated OTA control — the backstop that holds when a caller reaches the service directly, bypassing the CDN |
| `SERVICECONFIG_RATE_LIMIT_PER_MINUTE` | `300` | Sustained per-client budget | `<= 0` refuses to boot (when enabled). Too low 429s legitimate portal use |
| `SERVICECONFIG_RATE_LIMIT_BURST` | `60` | How far a client may run ahead of the sustained rate | `<= 0` refuses to boot (when enabled). A portal page load fires several requests at once; a burst below that fan-out 429s ordinary use |
| `DATABASECONFIG_*` | — | Owned by `u-common-components/database` | No database, no boot |
| `STORAGE_CONFIG_AWS_BUCKET_NAME`, `STORAGE_CONFIG_AWS_TOKEN_CRED_PATH` | — | Owned by `u-common-components/storage/v4` | Missing bucket name or credential **file** and the process will not boot at all, even though local never calls S3 |

The rate-limit trio moves together: budget = `PER_MINUTE` sustained,
`BURST` ahead. The limiter is **in-process**, so with N replicas the
effective budget is N× the configured one — acceptable for a
resource-exhaustion backstop, not a billing meter. Health probes are exempt
**by mounting**, not configuration: kubelet must never see a 429.

Two timeouts are constants in `main.go`, not config: `ReadHeaderTimeout` 10s
and `IdleTimeout` 120s — the slowloris backstop the chi Timeout middleware
cannot provide.

## 3. Database

### Migrations

Flyway, in `.db/`, named `V1.NN__description.sql` — **zero-padded** minor so
lexical and Flyway version order agree. `U1.NN__*.sql` undo files are
documentation only and are **never executed** (Flyway Community cannot run
`undo`). Forward-only in practice: write migrations so they never need
reversing.

```bash
brew install flyway
make db-migrate                          # drops + recreates u_l10n_migrate_test, applies from empty
make db-migrate DATABASECONFIG_HOST=... DATABASECONFIG_PORT=...
```

`db-migrate` proves the real Flyway is happy with the filename convention and
ordering — the schema tests replay the same files through `database/sql`.
Its `dropdb`/`createdb` carry the same `-h`/`-p` as everything else on
purpose: without them an overridden host would migrate one server while
`dropdb` destroyed a same-named database on another.

### Scratch databases

| Target | Database | Notes |
|--------|----------|-------|
| `make db-local` | `u_l10n_local` | Starts postgresql@14, creates if absent. Idempotent |
| `make db-local-stop` | — | Stops the brew service |
| `make db-test` | `u_l10n_test` | For the schema tests, which **DROP and recreate the public schema every run** — never point them at anything you value |
| `make db-migrate` | `u_l10n_migrate_test` | Dropped and recreated on every invocation |

### Integration tests

`make test` resolves a database in priority order: `TEST_DATABASE_URL` if
set; a testcontainers-managed `postgres:15.3-alpine` if Docker is reachable;
the local server at `postgres://localhost:5432/u_l10n_test` if listening.
With none of those the suite **skips**, so `go test ./...` stays green on a
machine without Docker.

**CI must set `REQUIRE_TEST_DB=1`**, which turns that skip into a hard
failure — a runner whose Docker is broken must not go green having run zero
tests.

## 4. Health and lifecycle

### Probes

| Endpoint | Question | Touches DB | Probe for | On failure Kubernetes… |
|----------|----------|------------|-----------|------------------------|
| `GET /healthz` | Is this process wedged? | **No** | liveness | restarts the pod |
| `GET /readyz` | Can this instance serve traffic now? | Yes (`PingContext`) | readiness | removes it from the Service, leaves it running |

Do not swap them. Liveness that pings the database turns a brief DB blip into
a fleet-wide simultaneous restart. `/readyz` answers 503 with
`{"status":"unavailable","checks":{"database":"unreachable"}}` when the DB is
down. Both sit outside `/api`, outside authentication, and outside the rate
limiter — kubelet presents no credentials and must never see a 429.

### Shutdown and timeouts

On SIGINT/SIGTERM the server stops accepting and drains in-flight requests
for at most `SERVICECONFIG_SHUTDOWN_TIMEOUT` (default 15s), then exits — with
an error if the drain deadline was exceeded. Keep the timeout below
`terminationGracePeriodSeconds`.

Timeout layers: `ReadHeaderTimeout` 10s and `IdleTimeout` 120s (constants,
server-level, catch slowloris); `SERVICECONFIG_REQUEST_TIMEOUT` 60s (chi
middleware, bounds handler execution and cancels the in-flight query through
context).

## 5. Incident playbook

### Rolling back a bad release (the kill switch)

```bash
# find the version: GET /api/v1/releases (newest first)
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://<gateway>/api/l10n/releases/<version>/rollback
```

Approver role. What it does: withholds the release from OTA serving. Clients
fall back to the newest earlier eligible release; when there is none they get
**410 Gone**, which tells the app to delete its cached bundle and use the
strings compiled into the binary (a 404 would not; a 200-with-empty-body
would actively break).

What it does **not** do: undo the values. Master keeps whatever the merge
applied — only the served bundle changes. Undoing the data is a separate act
(a branch with the old values, a merge), because a rollback happens in a
hurry and must not also rewrite the corpus.

- Propagation: OTA `Cache-Control: max-age=300`, so a CDN may serve the dead
  release for up to 5 minutes; negative answers carry `max-age=60`.
- Rolling back an already-rolled-back release is **409**, not a repeat —
  `rolled_back_by` must keep the name of whoever actually pulled it.
- Roll forward: fix master, then `POST /api/v1/releases` (manual publish,
  approver, ships master with no diff reviewed — `min_app_version` must be
  exactly three numeric components, e.g. `4.12.0`).
- Every rollback writes an `audit_events` row and a deliberately loud log
  line: `RELEASE ROLLED BACK — the OTA kill switch was pulled`.

### A bad merge

The record is complete; reconstruct before touching anything:

- `translation_history` and `key_history` hold every before/after with
  `source = 'merge'`, `branch_id`, `changed_by`, `version`, `changed_at`.
  A NULL `value` means "became untranslated"; `''` means "became blank" —
  they are different states.
- `audit_events` holds the structural actions (approve, merge, publish,
  rollback) with actor and request id.
- Release bundles are immutable —
  `GET /api/v1/releases/{version}/bundles/{locale}` shows exactly what
  shipped; the answer cannot drift after the fact.
- `merge_request_id` on the release is null for a manual publish — the first
  thing an incident review looks at: "somebody approved this" vs "somebody
  pushed it".

Recovery: roll back the release (above) to stop the bleeding, then create a
branch restoring the old values and merge it through review.

### Token compromise

```bash
u-l10n token revoke --name <token-name> --actor you@you.co
u-l10n token create --name <new-name> --scope read_export --actor you@you.co
```

Revocation is immediate: every request re-resolves the token by hash with
`revoked_at IS NULL` in the predicate — there is no cache to wait out. A
leaked u-l10n token is recognisable by its `ul10n_` prefix; match it to a
name via the stored `token_prefix` (first `ul10n_` + 6 chars). A revoked,
expired, or never-existed token all answer the same 401, so probing cannot
enumerate names.

### Rate-limit tuning

Symptoms: 429 with `{"error":"rate_limited"}` and a `Retry-After` header.
Tune the trio (`SERVICECONFIG_RATE_LIMIT_PER_MINUTE`, `_BURST`, `_ENABLE`) and
restart. Things to know before turning knobs:

- The key is the client IP, derived by walking `X-Forwarded-For` right to
  left past private/loopback hops — the first public IP as the edge saw it.
  Client-supplied leading XFF entries are never consulted. Fallback is the
  socket address; unparseable input shares one sentinel bucket.
- In-process: N replicas ⇒ N× the configured budget.
- The limiter runs **before** authentication, deliberately — the expensive
  paths it guards are pre-auth (an unseen bearer token costs an outbound
  Google call with a 10s timeout; an `X-Api-Token` costs a DB lookup).
- Bucket map is pruned at 4096 tracked clients, so address-spraying cannot
  grow it without bound.

### "OTA returns 500s" triage

The OTA handler has exactly two 500 paths, both logged: `ota: locale lookup
failed` and `ota: bundle lookup failed`. Both are database errors — the
min-app-version int[] cast hazard is closed at both ends (publish validates
the floor, the handler discards any `X-App-Version` that is not strict
`N.N.N`). So:

1. `curl /readyz` — if 503, this is the Postgres-unavailable case below.
2. Check the two log lines for the underlying DB error.
3. Distinguish from the non-errors: 404 `unknown_locale` (bad locale code),
   404 `no_release` (nothing published yet — the app uses its bundled
   strings), 410 `release_rolled_back` (kill switch, expected after a
   rollback with no earlier eligible release).
4. The CDN keeps serving cached 200s for up to `max-age=300`, so client
   impact lags the origin failure in both directions.

### Postgres unavailable

- `/readyz` answers 503 → pods leave the Service endpoints but are **not**
  restarted; `/healthz` stays 200, so no restart storm. Recovery is
  automatic when the database returns — nothing to do on the service side.
- While down: OTA and every `/api/v1` route 500/503 at the origin (API-token
  auth and user lookup both need the DB); the CDN keeps OTA clients on
  cached bundles up to 5 minutes; apps that get nothing fall back to their
  bundled strings — string delivery degrades, it does not break the app.
- Google-side outage is distinct: portal auth answers **503
  `identity_provider_unavailable`** (with `Retry-After: 5`), not 401 —
  operators should not be told to sign in again during someone else's outage.

## 6. Security operating rules

- **Never log a token, a presigned URL, or an asset URL** — they are
  credentials. The code logs at most a short prefix (`safePrefix` /
  `safeTokenPrefix`); keep it that way in anything you add.
- **Token plaintext exists once**, on stdout at `token create`, deliberately
  not logged (logs ship to a central store where a live credential must never
  land). Only the SHA-256 is stored; authentication is an indexed hash
  lookup, so the database never sees the secret either.
- **Prefer `$LOKALISE_API_TOKEN` over `--token`** — flags land in shell
  history and `ps` output.
- **Roles are ordered**: `viewer < editor < approver < admin`; unknown roles
  rank below viewer and fail closed. Reads are viewer, corpus writes are
  editor, merge/publish/rollback are approver, and only an **admin** grants
  roles (`PATCH /api/v1/admin/users/{email}/role`). The first admin can only
  come from the CLI (`u-l10n user grant`), run from the pod — which is
  already more privilege than any role expresses, and unlike `psql` it
  validates the role and writes the audit row.
- **`status = disabled` outranks the role column** — offboard by disabling,
  not by demoting; the role stays for the record.
- **The portal's `x-yp-role` header is ignored** by design; access decisions
  come from this service's `users` table via `GET /api/v1/me`.
- **API-token scopes are minimal**: `read_export` for pulling translations;
  `read_write` only for the asset endpoints — including the presigned GET,
  because context screenshots carry customer PII.
- **Asset views are audited before the URL is signed** — a failure to record
  who looked fails the request. Deliberately unlike the best-effort,
  minute-throttled `last_used_at` on API tokens.
