# u-l10n — testing

How the tests run, what the gates prove, what they deliberately cannot see, and how new tests should be written. Companion to [ARCHITECTURE.md](ARCHITECTURE.md).

## 1. Running the tests

```
make test                          # go test -v -cover ./...
go test -race ./...                # required before committing (CLAUDE.md Do/Don't)
REQUIRE_TEST_DB=1 go test ./...    # CI: turn the no-database skip into a failure
```

The integration suite (`integration-tests/`) needs a real PostgreSQL. `TestMain` resolves one in priority order:

1. `TEST_DATABASE_URL`, when set — an explicit override always wins.
2. A **testcontainers** container (`postgres:15.3-alpine`, matching u-reward), when a Docker daemon is reachable. The hermetic path: throwaway server per run, nothing shared, nothing left behind. The wait strategy requires the "ready to accept connections" log line **twice**, because Postgres logs it once during init and again when it opens for real.
3. The local development database at `postgres://localhost:5432/u_l10n_test`, if one happens to be listening.

If none of the three is available the whole package **skips silently** (`os.Exit(0)`), so `go test ./...` stays green on a machine without Docker or PostgreSQL. That silence is a hazard in CI: a runner whose Docker is broken would go green having run zero tests. `REQUIRE_TEST_DB=1` converts the skip into `os.Exit(1)`. CI must set it.

Migrations are applied once per run — `TestMain` drops the `public` schema and replays every `.db/V*.sql` in lexical order (the zero-padded minor versions make lexical order equal Flyway's version order). Tests never reset between cases; each keeps to its own key names. This is deliberate: it is how a test notices it has become order-dependent.

Scratch databases:

```
make db-test      # creates u_l10n_test for the fallback path; wiped every run
make db-migrate   # drops/recreates u_l10n_migrate_test and runs REAL Flyway from empty
```

The integration tests replay the migration files through `database/sql`; `make db-migrate` is what proves Flyway itself accepts the filename convention and ordering. Never point `TEST_DATABASE_URL` at anything you value — the suite drops the public schema.

## 2. The gates

`pkg/export/roundtrip_test.go` holds Gate R. It is an external test package (`export_test`) so it can import both `pkg/parse` and `pkg/export` without creating a production dependency between them — the two are independent implementations, and that independence is what makes the gate a real check rather than a tautology. The only place they meet is `toExport`, a deliberately dumb field copy.

- **R1 — semantic round-trip** (`TestGateR1_SemanticRoundTrip`). parse → serialize → parse preserves every key, value, and render hint exactly, for every committed u-mobile file in all three formats: 6 Flutter JSON + 8 Android XML + 8 iOS `.strings` = 22 files, ~98,000 values (the test logs the exact total; the pinned per-file counts currently sum to 98,119, and CLAUDE.md's Gates section recorded 98,320 at an earlier measurement — the figure moves with the corpus). Zero alterations tolerated. This is what guarantees no string is corrupted in an export.
- **R2 — idempotency** (`TestGateR2_SerializerIsIdempotent`). Feeding the serializer's own output back through parse → serialize produces byte-identical output. This is what makes future exports diff-clean; without it every export would produce a diff even when nothing changed. The XML leg deliberately includes `values-th`, the one Android file carrying a bare unescaped `"` (`TopUpMaxInfoMsg`).

**Byte-exactness against the committed u-mobile tree is explicitly NOT a goal.** It is not achievable, and the reason is a property of the files, not of this code: `/` escaping is inconsistent *within* every file (en-SG holds 369 escaped and 76 raw), and several files contain duplicate keys — four with conflicting values — which a store holding one value per key cannot represent. The committed tree is an artefact accreted from many Lokalise exports, not one coherent export. Byte-matching belongs against a *fresh* Lokalise export, where keys are unique by construction — that is the Stage 1 "Diff A" gate in the cutover runbook, not a test in this repo.

**What R1/R2 cannot see.** R1 compares the first parse against the re-parse — it never looks at the raw file. Parse-time fidelity loss is therefore invisible to it: if the parser corrupts a value on the way in (surrogate halves becoming U+FFFD, an unexpected `<plurals>` element silently dropped by `encoding/xml`, escaped and raw newlines conflated), both sides of the comparison carry the same corruption and the gate passes. The corpus tests in `pkg/parse/corpus_test.go` close that hole by pinning facts about the raw committed files, independently measured:

| Tripwire | Assumption it pins |
| --- | --- |
| `TestCorpusFlutterJSON` | Exact statement / distinct-key / empty-value counts per locale — nothing dropped, and explicitly-empty values counted as PRESENT, not absent. |
| `TestCorpusPerLocaleKeyPresenceDiffers` | en-MY genuinely has keys en-SG lacks — the evidence for the schema's three-state (absent ≠ empty) rule. |
| `TestCorpusAndroidXML` | Exact `<string>` counts, and entries == unique. `encoding/xml` silently ignores element types absent from the target struct; the count is what catches a vanished `<plurals>` or `<string-array>`. |
| `TestCorpusAndroidCDATA` | Exactly five values are CDATA-wrapped — a per-value render hint the exporter must reproduce. |
| `TestCorpusAndroidKeyTransform` | Android files carry TRANSFORMED key names (leading digits stripped, `[^A-Za-z0-9_]` → `_`); the canonical name does not appear. |
| `TestCorpusIOSStrings` | Exact statement / distinct-key counts per `.strings` file. |
| `TestCorpusIOSSurrogatePairEmoji` | The corpus really carries emoji as paired `\uXXXX` escapes; they must decode to the MY flag, never to U+FFFD runs. |
| `TestCorpusAndroidRawTripwires` — no escaped inline markup | No `&lt;b&gt;`-as-text exists; the parser's carve-out handles only raw `<b>`/`<u>`. |
| — non-b/u tags only inside CDATA | Every other inline tag (`<link>` ×3, `<A>` ×4, `<B>` ×4 — pinned so the test is known non-vacuous) lives inside CDATA; one outside would be re-escaped on export and change what the app renders. |
| — bare quotes pinned to one instance | aapt gives an unescaped `"` quote-section semantics, deliberately not handled in code; exactly one value (`values-th` `TopUpMaxInfoMsg`) relies on it, and a second must trip the wire before it ships. |
| — no numeric character references | Zero `&#dd;`/`&#xhh;` in the corpus; if one appears, the entity decoder becomes load-bearing and must be re-verified. |
| `TestCorpusIOSLineEndingsAreMixed` | Only the root `Localizable.strings` is CRLF (959 CRs, committed content) — the reason the `.strings` line ending is a serializer OPTION, not a constant. |
| `TestCorpusNewlineFormsCoexist` | Real newlines and literal `\n` coexist in quantity in en-SG and the parser keeps them distinct — conflating them corrupts ~1,050 strings that look identical in every editor. |
| `TestCorpusDuplicateKeys` | Exact duplicate-key counts per file, and the dangerous subset with CONFLICTING values — the fact that bounds Gate R and forces the importer to collapse last-wins with a warning. |

The raw-text tripwires scan the file bytes, deliberately not the parser's output — the output cannot distinguish an escaped quote from a bare one once unescaping has run.

The third load-bearing gate is **concurrency**: the advisory lock serialises merges, verified by *removing* the lock and watching the test fail (section 4).

## 3. Corpus pinning

Every count in `corpus_test.go` was measured **independently of the parser** — Python `json.load` (with `object_pairs_hook` to keep duplicate statements) for Flutter, ElementTree for Android, a regex scan for iOS — against the u-mobile working tree. They are a tripwire, not a specification.

The corpus is the committed `FE/u-mobile` checkout, expected at `../../../../FE/u-mobile` relative to `pkg/parse` (override with `U_MOBILE_PATH`). Both the corpus tests and Gate R **skip** when it is not checked out. That tree moves independently of this repo: a red corpus test after a re-pull is more likely upstream drift than a parser bug.

The procedure when one goes red:

1. Confirm the u-mobile checkout moved (it usually has).
2. Re-measure the affected counts **independently** — the same Python tooling, never the parser under test.
3. Update the pinned values in the same commit, with a dated comment explaining why they moved and what was checked. Precedent is in the file: the 2026-08-09 re-measure after u-mobile moved to `0e2fe9190` notes that only the six Flutter files changed and that the Android/iOS counts and duplicate-key counts were verified unaffected.

Never "fix" a red corpus test by copying the parser's own output into the expectation — that turns an independent measurement into a tautology.

## 4. The mutation-check doctrine

CLAUDE.md, Coding Principles: **a test never seen red is indistinguishable from one that cannot fail.** Any test whose subject is a refusal — a constraint, a lock, an ordering — must be proven capable of failing, by mutating the thing it guards and watching it go red.

Suites that carry this proof:

- **The advisory lock.** `TestConcurrentMergesSerialise` (`integration-tests/concurrency_test.go`) races two approved merge requests through the real `mrsvc.Merge` path. Its red-verification is manual: remove the `SELECT pg_advisory_xact_lock(?)` call in `pkg/service/mergesvc/merge.go` (line ~131) and run the test — without the lock, both transactions compute `max(version) + 1` from the same snapshot and one dies on the releases unique index (23505), deadlocks (40P01), or interleaves its conflict computation with the other's writes. The test cannot mutate its own production code, so this is a manual gate step, re-run whenever the merge transaction changes. `TestMergeLockKeyMatchesTheMergeService` pins the test's key to `mergesvc.MergeLockKey()` — without that pin, the lock-mechanics test could pass forever against a key nothing in production uses, the exact tautology this file once had.
- **`requireRejected`'s class-23 contract** (`integration-tests/helpers_test.go`). "Any error" is not good enough: a typo'd column name fails with SQLSTATE 42703 and would satisfy a bare nil-check, turning a broken test into a passing one. When a `*pq.Error` is in the chain it must be class 23 (23502/23503/23505/23514 — an integrity constraint actually firing). A chain with no `*pq.Error` means the repository mapped the refusal to a typed sentinel (e.g. `ErrTagNameTaken`), and such call sites assert the specific sentinel themselves.
- **Rate-limit mount order** (`route/ratelimit_test.go`, `TestRateLimitMountedBeforeAuth`). Driven through the real `ProvideRoutes` router with nil services, so the mounting order itself is under test: garbage credentials must be refused by the limiter *before* the expensive pre-auth work, and the health probes must stay exempt (a kubelet that gets 429 from a liveness check restarts a healthy pod under load). Per the PR that introduced it, this was red-verified by mounting the limiter after auth and watching the test fail.

To red-verify the concurrency test yourself:

```
# comment out the pg_advisory_xact_lock line in pkg/service/mergesvc/merge.go, then:
go test ./integration-tests/ -run TestConcurrentMergesSerialise -count=5
# expect 23505 / 40P01; restore the lock, expect green
```

The assertion in that test is on **outcomes** (both merges succeed, distinct release versions, both edits on master), not on timing windows — the merge holds the lock only as long as its own work takes, so a wall-clock overlap assertion would be a scheduling lottery. The timing-window assertion lives in `TestAdvisoryXactLockMechanicsSerialiseCriticalSections`, which pins the Postgres primitive rather than the merge path.

## 5. Conventions for new tests

- **testify** (`assert`/`require`) everywhere; **testcontainers** for anything that needs PostgreSQL. No new test frameworks.
- **Test that constraints FAIL, not just that writes succeed.** Use `requireRejected` for database refusals; assert the typed sentinel for repository-level refusals. A constraint nobody has watched reject anything is a constraint nobody can trust.
- **Failing test first for bug fixes.** Write the test that goes red against the current code, then fix it (CLAUDE.md Do/Don't).
- Repository and service tests drive the **real** GORM path via `testGORM` — the repositories speak GORM, and driving them through `database/sql` would test a different code path from production. The shared handle is opened once per package (one pool per test would exhaust `max_connections`).
- The package never resets state between cases: use `uniqueName`, and quarantine anything globally visible you create — merges and publishes roll their releases back (`mergeAndQuarantine`, `publishAndQuarantine`), because the OTA tests assert on exactly which release a locale serves.
- Don't let `pkg/parse` import `pkg/export` or vice versa; round-trip tests go in the external `export_test` package.

### Integration test file map

| File | Covers |
| --- | --- |
| `helpers_test.go` | Database provisioning, migration replay, fixtures, `testGORM`, `requireRejected`. |
| `schema_test.go` | The schema's constraints reject bad input: active-only key-name uniqueness, three-state translations, OCC, locale seed pinned to u-mobile's `run.sh` export paths. |
| `branch_cow_test.go` | Copy-on-write branch semantics, in the SQL `pkg/repository/branch.go` issues. |
| `branchsvc_test.go` | Branch service: diff agrees with the merge about conflicts, tombstones ≠ blanks, lifecycle refusals and audit. |
| `keysvc_test.go` | Key service over the real schema: three-state reads end to end, branch reads vs master, 409s carrying both values, delete removes rather than blanks. |
| `keymeta_test.go` | The second conflict type (key metadata, anchored on `keys.version`) and the third (name collisions); soft-delete merging as a status change. |
| `conflict_test.go` | The single conflict rule covers every race; later edits invalidate approvals; resolutions unique per conflict. |
| `merge_test.go` | The merge transaction's SQL: clean deltas apply, concurrent master writes refuse, tombstones delete, release versions monotonic, bundle SHA describes the served bytes. |
| `mrsvc_test.go` | Merge-request workflow: state transitions, approval invalidation, refusal with the conflict list attached, a successful merge cuts a release. |
| `concurrency_test.go` | The advisory lock (primitive and real merge path), lock ordering vs deadlock, OCC under real contention. |
| `releasesvc_test.go` | Publish materialises every locale in one transaction; rollback is a kill switch, not an undo; audit. |
| `ota_test.go` | OTA serves the newest eligible release; version floor compares numerically. |
| `asset_repo_test.go` | Asset and audit SQL, mirrored by hand from `pkg/repository/asset.go` / `audit.go`. |
| `tag_repo_test.go` | The real `TagRepository` (not mirrored SQL): `SetKeyTags` replaces, bulk writes behave. |
| `tagsvc_test.go` | Tag service rules and audit trail. |
| `user_repo_test.go` | User upsert's `ON CONFLICT` and CITEXT case-insensitivity — load-bearing for authorization, checked by no compiler. |
