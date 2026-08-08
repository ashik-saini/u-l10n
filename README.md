# u-l10n

In-house localization service — the source of truth for mobile translation keys,
the byte-exact export engine, and (later) over-the-air string delivery.

Replaces [Lokalise](https://lokalise.com) for the YouTrip mobile apps: ~6,300
keys across 6 locales, exported to Flutter JSON, Android XML and iOS `.strings`.

**Design documents**

- [u-l10n Backend Guide — From First Principles to Production](https://yougroup.atlassian.net/wiki/spaces/~712020c47284976f8344aa936d47c1896850fd/pages/5320474635) — architecture, database, and the rationale behind every decision here
- [In-house Localization System Development Plan](https://yougroup.atlassian.net/wiki/spaces/~712020c47284976f8344aa936d47c1896850fd/pages/5295014013) — scope, phases, cutover runbook

## Status

**Phases 0–3 complete; Phase 7 partial.** The service boots, owns its schema,
reads and writes all three mobile localization formats, serves authenticated
exports, seeds from the committed u-mobile tree, and folds branch edits into
master behind an approval workflow.

| Phase | State | Gate |
|-------|-------|------|
| 0 scaffold | done | probes verified against a real database, including DB-down and recovery |
| 1 schema | done | Flyway from empty; 27 subtests proving constraints *reject* bad input |
| 2 parsers + seed | done | all 22 committed files at exact counts; 7,516 keys / 35,872 translations, idempotent |
| 3 serializers + export | done | **R1: 98,320 values round-tripped, zero alterations**; R2 idempotent; export endpoint behind API tokens |
| 7 branch + merge | done | COW deltas, **all three conflict types**, merge transaction, releases — verified under `-race` |

**Not yet implemented:** Phases 4 (Lokalise importer), 5 (portal API),
6 (assets/S3), 10 (OTA) and 11 (mobile SDK).

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
| `DATABASECONFIG_*` | — | Owned by `u-common-components/database` |

Invalid configuration fails at startup rather than at first request — a service
that boots with bad config only defers the outage.

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
