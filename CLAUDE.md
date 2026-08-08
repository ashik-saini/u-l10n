# CLAUDE.md

Guidance for Claude Code (http://claude.ai/code) when working in this repository.

## What This Is

u-l10n is the in-house localization service, replacing Lokalise for the YouTrip
mobile apps: ~6,300 translation keys across 6 locales, exported byte-faithfully
to Flutter JSON, Android XML and iOS `.strings`. It owns the source of truth,
serves the portal API, and delivers strings over the air so copy changes reach
users without an app release.

HTTP only on `:8080`. No gRPC, no Kafka.

## Stack

Go 1.23, chi v4 (HTTP router), PostgreSQL (GORM v1 + hand-written SQL), Google
Wire (DI), urfave/cli (CLI), Flyway (migrations), S3 via
`u-common-components/storage/v4`, testcontainers (integration tests).

The master plan originally specified chi v5, pgx, goose, cobra and slog. All
five were wrong — none appear anywhere in `BE/`. If a doc and this file
disagree about the stack, this file is right.

## Commands

```
make test           # unit + integration; provisions PostgreSQL via Docker, or skips
make db-test        # scratch database for the schema tests
make db-migrate     # apply migrations with real Flyway, from empty
make run-local      # start PostgreSQL and run the service
make seed-local     # load the committed u-mobile tree (DRY_RUN=--dry-run to rehearse)
make gen-wire       # regenerate wire_gen.go  — NOTE: `wire .`, not `wire ./...`
make s3-cred-local  # placeholder credential file storage/v4 refuses to start without
```

## Architecture

```
main.go                    urfave/cli: serve | seed-from-files | import | token | user
inject_service.go          Wire providers  →  wire_gen.go (generated, never edit)
route/                     chi handlers, one file per endpoint group
pkg/config/                configstruct env config
pkg/model/                 pure types, zero imports
pkg/repository/            ALL SQL lives here
pkg/service/               business rules; owns transaction boundaries
pkg/parse/  pkg/export/    the two independent format implementations
pkg/lokalise/              rate-limited API client
pkg/googleauth/            Google tokeninfo verifier
.db/                       Flyway migrations
integration-tests/         schema + repository tests against real PostgreSQL
```

## The five things that will bite you

Read these before changing anything. Each caused a real bug here.

**1. A (key, locale) pair has THREE states, not two.**
No row = untranslated, omitted from exports. `value = ''` = deliberately blank,
exported as `""`. Anything else = translated. Collapsing absent into empty adds
~430 spurious keys to en-SG; collapsing empty into absent deletes 3,664
intentional blanks from ms-MY. **Never return a bare `string`** from a
repository — Go's zero value is `""`, which silently merges the two. Use
`(value string, found bool, err error)`.

**2. `WithTransaction` does NOT nest.**
A repository opening its own transaction inside a caller's silently splits the
work across two, with no error raised. Services own transaction boundaries;
every repository method takes an optional `tx *gorm.DB` and uses the
`db(ctx, tx)` helper. This is why the merge transaction's atomicity holds.

**3. `base_master_version` is captured on FIRST touch only.**
It records where a branch started, not what it has done since. The
`ON CONFLICT` branches deliberately do not update it. Refreshing it would let a
branch adopt master's newer version as its base, and a genuine conflict would
look clean.

**4. GORM v1 has no `clause.OnConflict` or `clause.Locking`.**
Those are v2 APIs. Use hand-written SQL through `Exec`/`Raw` for `ON CONFLICT`,
and `Set("gorm:query_option", "FOR UPDATE")` for row locks.

**5. Real newlines and literal `\n` are different values.**
en-SG alone holds 153 of the first and 897 of the second, and they render
identically in every editor. Unescaping must be single-pass; a chain of
`ReplaceAll` turns a literal backslash-n into a real newline and corrupts ~1,050
strings.

## Conventions

- **Logging** `ulog.GetLogger("u-l10n")` → `log.Infow(ctx, msg, k, v)`,
  `log.Errore(ctx, msg, err)`. Never log a token, a presigned URL, or an asset
  URL — they are credentials.
- **Errors** exported sentinels matched with `errors.Is`, mapped to HTTP status
  in the handler. Caller mistakes are 4xx. Only genuinely unexpected failures
  reach 500.
- **Migrations** Flyway, `.db/V1.NN__description.sql`, zero-padded minor so
  lexical and version order agree. `U1.NN__` undo files are documentation only
  and are **never executed** — Flyway Community cannot run `undo`. Forward-only
  in practice.
- **Layers** `route/` decodes and maps to status codes; `pkg/service/` decides;
  `pkg/repository/` speaks SQL; `pkg/model/` imports nothing.
- **Query parameters** reject unknown ones rather than ignoring them — a client
  that misspells one must be told.
- **Tests** testify; testcontainers for integration. Test that constraints
  **fail**, not just that inserts succeed.

## Do / Don't

- DO run `gofmt -w . && go build ./... && go vet ./... && go test -race ./...`
  before committing.
- DO regenerate with `wire .` after changing providers — `wire ./...` fails on
  packages with no directives.
- DO write the test that fails against the current code first, when fixing a bug.
- DON'T edit `wire_gen.go` by hand.
- DON'T let `pkg/parse` import `pkg/export` or vice versa. They are two
  independent implementations, and that independence is what makes the
  round-trip gate a real check rather than a tautology.
- DON'T add `aws-sdk-go-v2` — it is indirect-only across the whole tree; use
  `storage/v4`.
- DON'T assume figures from the design docs. Several were stale and were
  corrected by measurement: Flutter key counts, escape counts, and the claim
  that the JSON files carry no trailing newline (they do).

## Gates

Each phase has a gate that must pass. The load-bearing ones:

- **R1** parse → serialize → parse preserves every key and value exactly.
  98,320 values across 22 files, zero alterations.
- **R2** the serializer is idempotent, so exports are diff-clean.
- **Concurrency** the advisory lock serialises merges; verified by *removing*
  the lock and watching the test fail.

Byte-exactness against the committed u-mobile files is **not** achievable —
`/` escaping is inconsistent within every file and several contain duplicate
keys. That check belongs against a fresh Lokalise export (Diff A), not this tree.

## Coding Principles

1. **Verify, don't inherit.** Measure the corpus rather than trusting a figure
   in a document. Four plan assertions were disproved this way.
2. **Prove the test can fail.** A concurrency or constraint test never seen red
   is indistinguishable from one that cannot go red.
3. **Fail loudly over silently.** A refused merge beats a partially applied one;
   a 400 beats a default; a missing export file is fatal, not a warning.
4. **Simplicity first.** Minimum code that solves the problem. No speculative
   features, no abstractions for single-use code.
5. **Surgical changes.** Touch only what you must. Match existing style. Don't
   "improve" adjacent code.

<!-- code-review-graph MCP tools -->
## MCP Tools: code-review-graph

**IMPORTANT: This project has a knowledge graph. ALWAYS use the
code-review-graph MCP tools BEFORE using Grep/Glob/Read to explore
the codebase.** The graph is faster, cheaper (fewer tokens), and gives
you structural context (callers, dependents, test coverage) that file
scanning cannot.

### When to use graph tools FIRST

- **Exploring code**: `semantic_search_nodes_tool` or `query_graph_tool` instead of Grep
- **Understanding impact**: `get_impact_radius_tool` instead of manually tracing imports
- **Code review**: `detect_changes_tool` + `get_review_context_tool` instead of reading entire files
- **Finding relationships**: `query_graph_tool` with callers_of/callees_of/imports_of/tests_for
- **Architecture questions**: `get_architecture_overview_tool` + `list_communities_tool`

Fall back to Grep/Glob/Read **only** when the graph doesn't cover what you need.

### Key Tools

| Tool | Use when |
| ------ | ---------- |
| `detect_changes_tool` | Reviewing code changes — gives risk-scored analysis |
| `get_review_context_tool` | Need source snippets for review — token-efficient |
| `get_impact_radius_tool` | Understanding blast radius of a change |
| `get_affected_flows_tool` | Finding which execution paths are impacted |
| `query_graph_tool` | Tracing callers, callees, imports, tests, dependencies |
| `semantic_search_nodes_tool` | Finding functions/classes by name or keyword |
| `get_architecture_overview_tool` | Understanding high-level codebase structure |
| `refactor_tool` | Planning renames, finding dead code |

### Workflow

1. The graph auto-updates on file changes (via hooks).
2. Use `detect_changes_tool` for code review.
3. Use `get_affected_flows_tool` to understand impact.
4. Use `query_graph_tool` pattern="tests_for" to check coverage.
