# u-l10n

In-house localization service — the source of truth for mobile translation keys,
the byte-exact export engine, and (later) over-the-air string delivery.

Replaces [Lokalise](https://lokalise.com) for the YouTrip mobile apps: ~6,300
keys across 6 locales, exported to Flutter JSON, Android XML and iOS `.strings`.

**Design documents**

- [u-l10n Backend Guide — From First Principles to Production](https://yougroup.atlassian.net/wiki/spaces/~712020c47284976f8344aa936d47c1896850fd/pages/5320474635) — architecture, database, and the rationale behind every decision here
- [In-house Localization System Development Plan](https://yougroup.atlassian.net/wiki/spaces/~712020c47284976f8344aa936d47c1896850fd/pages/5295014013) — scope, phases, cutover runbook

## Status

**Phases 0–3 and 6 complete; Phase 7 partial.** The service boots, owns its
schema, reads and writes all three mobile localization formats, serves
authenticated exports, seeds from the committed u-mobile tree, stores context
screenshots on S3, and folds branch edits into master behind an approval
workflow.

| Phase | State | Gate |
|-------|-------|------|
| 0 scaffold | done | probes verified against a real database, including DB-down and recovery |
| 1 schema | done | Flyway from empty; 27 subtests proving constraints *reject* bad input |
| 2 parsers + seed | done | all 22 committed files at exact counts; 7,516 keys / 35,872 translations, idempotent |
| 3 serializers + export | done | **R1: 98,320 values round-tripped, zero alterations**; R2 idempotent; export endpoint behind API tokens |
| 5a identity | done | **every refusal path tested**: unknown email and disabled account are 403 not 401, unknown roles rank below viewer, the cache never outlives a token |
| 6 assets | done | presign/confirm/attach against a fake object store; **every refusal path tested**; repository SQL against real PostgreSQL |
| 7 branch + merge | done | COW deltas, **all three conflict types**, merge transaction, releases — verified under `-race` |

**Phase 4 (Lokalise importer)** is built but unexercised against the real API —
it needs a read-scope token (master plan open item #4). The client and the
presence-oracle logic are covered by tests against a fake server.

**Phase 6 is unexercised against a real bucket.** There is no local S3 or MinIO
anywhere in this tree and `storage/v4` hardcodes TLS with no path-style option,
so the S3 half is proven only against a fake — see [Context screenshots](#context-screenshots).

**Phase 5a is unexercised against a real Google token.** The verifier is driven
entirely against a fake tokeninfo endpoint — see [Portal identity](#portal-identity).

**Not yet implemented:** Phase 5b (the portal CRUD endpoints) and 11 (mobile SDK
— the Flutter client for the OTA endpoint).

## Quick start

Requires Go 1.23+ and PostgreSQL. Both the database and the service start with:

```
make run-local
```

That starts Homebrew PostgreSQL, creates `u_l10n_local` if absent, and runs the
service on `:8080`. Then:

```
curl -s localhost:8080/healthz | jq   # {"status":"ok","service":"u-l10n"}
curl -s localhost:8080/readyz  | jq   # {"status":"ready","checks":{"database":"ok"}}
```

| Command | Does |
|---------|------|
| `make test` | unit tests + schema tests (provisions its own PostgreSQL via Docker, or skips) |
| `make db-test` | create the scratch database the schema tests use |
| `make db-migrate` | apply the migrations with real Flyway, from empty |
| `make run-local` | start PostgreSQL and run the service |
| `make s3-cred-local` | write the placeholder S3 credential file `storage/v4` refuses to start without |

## Health probes

The two probes answer deliberately different questions, and conflating them
causes outages.

| Endpoint   | Question | Touches DB | On failure Kubernetes… |
|------------|----------|------------|------------------------|
| `/healthz` | Is this process wedged? | **No** | restarts the pod |
| `/readyz`  | Can this instance serve traffic now? | Yes | removes it from the Service, leaves it running |

If liveness pinged the database, a brief database blip would restart every pod
in the fleet simultaneously — turning a recoverable dependency outage into a
self-inflicted one. Readiness is the correct place for dependency checks: stop
sending work, let it recover, restart nothing.

## Database

Migrations are Flyway, in `.db/`, named `V1.NN__description.sql`. Minor
versions are **zero-padded** so lexical and Flyway version ordering agree —
without it `V1.9` would sort after `V1.10`.

`U1.NN__*.sql` undo files exist for parity with the other services but **are
never executed**: Flyway Community cannot run `undo`, and no script in the
deploy pipeline invokes it. Flyway ignores them outright. Treat migrations as
forward-only and write them so they never need reversing.

Three details in the schema are load-bearing and easy to "simplify" by
accident:

- **A (key, locale) pair has three states, not two.** No row = untranslated and
  omitted from the export; `value = ''` = deliberately blank and exported as
  `""`; anything else = translated. Collapsing absent into empty adds ~430
  spurious keys to en-SG; collapsing empty into absent deletes 3,664
  intentional blanks from ms-MY. The repository layer must therefore never
  return a bare `string` — presence and content are separate facts.
- **`keys.name` is unique only among `status = 'active'`** (a partial index).
  Without the predicate, soft-deleting a key would reserve its name forever.
- **`branch_translations.base_master_version`** records what master's version
  was when a branch *first* touched a pair, and is never updated afterwards. It
  reduces the entire value-conflict rule to one comparison.

The seeded locale directory names come from `u-mobile/scripts/l10n/run.sh` and
are pinned by a test. Note en-SG's Android directory is bare `values`, not
`values-en-rSG`.

## Context screenshots

Screenshots that tell a translator what a string looks like in the app. They
never reach an export and never enter an OTA bundle.

Upload is a three-step handshake, and the middle step does not touch this
service:

| Step | Call | What it does |
|------|------|--------------|
| 1 | `POST /api/v1/assets/presign` | validates the declaration, returns a signed POST form — or the existing asset, with no upload URL, if those exact bytes are already stored |
| 2 | browser → S3 | the bytes go straight to the bucket |
| 3 | `POST /api/v1/assets/confirm` | HEADs the object, checks it against the declaration, and only then inserts the row |

Then `PUT /api/v1/keys/{id}/assets` and `DELETE /api/v1/keys/{id}/assets/{assetId}`
attach and detach, and `GET /api/v1/assets/{id}/url` issues a short-lived
presigned GET. Every one of them requires a `read_write` token — including the
GET, because these images carry customer PII and a token issued to pull
translations has no business reading them.

Four details are load-bearing:

- **The upload is a POST policy, not a presigned PUT.** `SignURL`'s PUT branch
  ignores `Options.ContentType` and attaches no conditions at all, so a browser
  could upload anything of any size to the key we signed and every validation
  would be decorative. Only the POST branch signs conditions S3 itself enforces.
  Note that branch derives the content type from the *filename argument's
  extension*, not from `Options.ContentType`, which it ignores too — so the
  filename it is given is derived from the validated content type, and the
  caller's own filename travels separately as metadata.
- **Confirm never trusts the client.** The declaration is bound into the signed
  policy as `x-amz-meta-declared-*`, and confirm reads it back off the object
  and compares it against the object's real size and content type. The policy
  cannot pin the size — `storage/v4` sets no content-length-range — so that
  comparison is the only thing standing between a 4KB declaration and a 50MB
  upload.
- **Every view and every attach writes an `audit_events` row**, and for a view
  the row is written *before* the URL is signed: a failure to record who looked
  fails the request. That is deliberately unlike the best-effort `last_used_at`
  on API tokens.
- **`image/webp` is rejected at presign** with a 400 that says why. The schema
  permits it, but `storage/v4`'s content-type table does not know the extension
  and signing fails outright, which would surface as a 500.

There is no local S3 or MinIO anywhere in this tree and `storage/v4` hardcodes
TLS with no path-style option, so the object store cannot be pointed at a
double. The service therefore declares its own two-method `ObjectStore`
interface — the same move as `route.Pinger` — and the tests drive the refusal
paths against a fake. **The S3 calls themselves have never run against a real
bucket.**

## Portal identity

Two kinds of principal reach this service and they authenticate differently.
Scripts and CI present `X-Api-Token` (`route.RequireAPIToken`); human operators
present a Google access token as `Authorization: Bearer …`
(`route.RequireIdentity`). The two are separate context keys, so no code
downstream can confuse a script for a person.

Only half of bo-api's `AccessTokenFilter` is ported. bo-api validates the opaque
token against Google's tokeninfo endpoint *and then* queries the Google Admin
Directory API to derive `yp_*` group permissions. u-l10n takes the first step
and stops: roles live in its own `users` table keyed on email, because a
designer may be an l10n editor and nothing else, and overloading another
system's authorization model means inheriting its every future change. So there
is no service account, no domain-wide delegation and no Directory API scope
here. The `x-yp-role` header the portal also sends is ignored outright — it
describes YouPortal, and it arrives from the client, which makes it a request
rather than a fact. **The portal must gate its UI on `GET /api/v1/me`.**

Four details are load-bearing:

- **Authentication failure is 401; authorization failure is 403.** A valid
  Google token whose email has no row in `users`, or whose row is
  `status = 'disabled'`, gets 403 — not 401. Conflating them sends a person
  without an account round the sign-in loop forever. Only the 401 carries
  `WWW-Authenticate`; a 403 that invites re-authentication is a lie.
- **Roles are ordered — `viewer < editor < approver < admin` — and unknown
  roles rank below viewer.** Ordering lets a route state the minimum it needs
  instead of enumerating every role that qualifies. Ranking the unknown *below*
  the floor is what makes an unexpected value fail closed: a role added to the
  database ahead of the code that understands it grants nothing. An unknown
  *minimum* is satisfied by nobody either, so a typo in a route definition
  refuses everyone rather than admitting everyone.
- **Verifications are cached in-process for the token's own remaining TTL,
  capped at five minutes.** Without a cache every portal request costs a round
  trip to Google and a six-panel page pays six times. The cap bounds how long a
  token revoked at Google keeps working here; taking the *minimum* of the two is
  what stops the cache from quietly extending a credential's life. The map is
  keyed on the SHA-256 of the token, never the token.
- **Google being unreachable is a 5xx, not a 401.** The token may be perfectly
  good, and answering 401 during someone else's outage tells every operator in
  the building to sign in again.

### Bootstrapping

An empty `users` table contains no admin, so no authenticated request can ever
create the first one — `PATCH /api/v1/admin/users/{email}/role` requires exactly
the role nobody holds yet. Something outside the authorization system has to
start the chain:

```
u-l10n user grant --email you@you.co --role admin --actor you@you.co
u-l10n user list
```

The shell is the right place for it. Running this requires access to the pod and
its database credentials — strictly more privilege than any role in the table
can express — so anyone who can run it could already have written the row by
hand with `psql`. Unlike `psql`, it validates the role and writes an
`audit_events` row, and it shares its code path with the API so the two cannot
drift apart on what a valid role is. The alternatives are worse: an admin
address seeded by a migration is a credential in git nobody remembers to remove,
and an env-var superuser is a permanent invisible bypass of the whole role model.

### What is not proved

The Google call itself has never run against Google. `pkg/googleauth` is driven
against a fake tokeninfo server, which is why it declares an interface for the
verifier at all — the same move as `route.Pinger` and `assetsvc.ObjectStore`.
The response shape, the `expires_in` semantics and the exact status code Google
returns for an expired token are taken from bo-api's long-running use of the
same endpoint, not from an observation made here.

`SERVICECONFIG_GOOGLE_OAUTH_AUDIENCE` is unset by default and the audience check
is skipped when it is empty. That matches what bo-api does today, but it is a
real gap: any Google OAuth client can mint an access token for a `you.co` user
and tokeninfo will validate it, so an unrelated application's token is accepted
here as proof of intent to use u-l10n. Set it everywhere the portal runs.

## Layout

```
main.go              urfave/cli entrypoint, HTTP server, graceful shutdown
inject_service.go    Wire provider graph  →  wire_gen.go (generated)
route/               chi handlers — one file + one _test.go per endpoint
pkg/config/          service configuration (env vars via configstruct tags)
.db/                 Flyway migrations
integration-tests/   schema tests against a real PostgreSQL
```

Layer discipline — when unsure where code belongs, match one of these sentences:

| Layer | Does | Must never |
|-------|------|------------|
| `route/` | decode, validate, map errors to status codes | hold business rules or SQL |
| `pkg/service/` | business rules; owns the transaction boundary | know about `http.Request` |
| `pkg/repository/` | SQL, row↔struct mapping; accepts an optional `tx` | open its own transaction |
| `pkg/model/` | types | import anything |

The last rule is not stylistic: the shared `Transactional.WithTransaction`
helper **does not nest** — a nested call opens a second, independent
transaction and silently breaks atomicity.

## Configuration

All configuration comes from environment variables. In Kubernetes these arrive
from a ConfigMap via `envFrom`; locally the Makefile supplies them.

| Variable | Default | Purpose |
|----------|---------|---------|
| `SERVICECONFIG_ENV` | `local` | Deployment environment |
| `SERVICECONFIG_HTTP_PORT` | `8080` | HTTP listen port |
| `SERVICECONFIG_REQUEST_TIMEOUT` | `60s` | Per-request deadline; cancels in-flight queries |
| `SERVICECONFIG_SHUTDOWN_TIMEOUT` | `15s` | Drain window on SIGTERM; keep below `terminationGracePeriodSeconds` |
| `SERVICECONFIG_GOOGLE_OAUTH_AUDIENCE` | *(empty)* | Portal OAuth client id. When set, access tokens issued to any other client are refused — see [Portal identity](#portal-identity) |
| `DATABASECONFIG_*` | — | Owned by `u-common-components/database` |
| `STORAGE_CONFIG_AWS_*` | — | Owned by `u-common-components/storage/v4` |

Invalid configuration fails at startup rather than at first request — a service
that boots with bad config only defers the outage.

`storage/v4` takes that further than the rest: it requires a bucket name *and*
reads its credentials from a file, and returns an error from either if they are
missing — so since Phase 6 the process will not boot without them, even though
nothing local ever calls S3. `make run-local` generates a placeholder credential
file (`make s3-cred-local`) to get past that check. It is gitignored and it is
not a credential.

## Code generation

`wire_gen.go` is generated; never edit it. Regenerate with `make gen-wire`.

The wire *library* is pinned to v0.5.0 to match the other services, but the
*generator binary* must be v0.6.0+ to parse modern Go — v0.5.0 vendors a 2019
copy of `x/tools` that panics on Go 1.24+. The two versions are independent
because `wire_gen.go` does not import wire at runtime.

```
go install github.com/google/wire/cmd/wire@latest
```

## Conventions

Follows the house standards used across `BE/`, verified against `u-reward`:

- **Router** chi v4 + `go-chi/render`; **DI** Google Wire; **CLI** urfave/cli
- **Logging** `ulog.GetLogger` — `Infow(ctx, msg, k, v)`, `Errore(ctx, msg, err)`
- **Config** `configstruct` / `configdefault` struct tags
- **Migrations** Flyway, `.db/V1.NN__description.sql`, forward-only in practice
- **Tests** testify; testcontainers for integration tests

Commits follow [Conventional Commits](https://www.conventionalcommits.org).

## Known gaps

Deliberately deferred, not forgotten:

- **No `vendor/`.** House convention vendors dependencies for the Docker build.
  Added when the Dockerfile and CI pipeline land — vendoring now would make the
  first review thousands of files of noise.
- **No `.container/`, `.buildkite/`, `.kubernetes/`.** These are largely
  `do-tools`-generated and will be wrong until infra provisions the service.
- **Schema tests provision their own PostgreSQL.** The suite resolves a database
  in priority order: `TEST_DATABASE_URL` if set, otherwise a testcontainers-managed
  `postgres:15.3-alpine` when Docker is reachable, otherwise a local server if one
  is listening. With none of those it skips rather than fails, so `go test ./...`
  stays green on a machine without Docker.
