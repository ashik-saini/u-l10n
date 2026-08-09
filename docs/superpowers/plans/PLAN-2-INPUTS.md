# Plan 2 inputs — carried forward from the foundation branch

Written at the end of `feat/multi-project-foundation` (Plan 1 of 3). Everything here was found by review during that work and is deliberately NOT in the branch. Plan 2 is the scoping sweep: threading project scope explicitly through the repositories, switching auth to per-project roles, moving portal routes under `/api/v1/projects/{project}/`, and making the merge lock per-project.

## Start here: audit what the migrations un-guaranteed

**The single most important lesson from Plan 1.** Its unit of work was "add `project_id` and a composite foreign key" and it never included "audit every query that relied on a uniqueness we are dropping." Four of five affected queries went undocumented, and the most destructive of them survived seven per-task reviews.

Drive this audit off the `DROP CONSTRAINT` and `DROP INDEX` lines in `V1.10`–`V1.12`, **not** off a `TODO(plan-2)` grep — the whole problem is the sites nobody thought to mark.

Known members of that family, all marked `TODO(plan-2)` in code:

| Site | Lost guarantee | Consequence |
|---|---|---|
| `release.go` `rollbackSQL` | `releases_version_unique` (V1.12) | **Destructive.** No project predicate and no `LIMIT`, so a rollback of version 1 hits *both* projects. `RowsAffected` is 2, which passes the `== 0` check, so the handler reports success while withdrawing shipped copy from every client. |
| `release.go` `ByVersion` | same | first-row-wins across projects |
| `branch.go` `ByName` | `branches_name_unique` (V1.11) | every portal branch route resolves through it |
| `key.go` name lookup | `idx_keys_name_active` rebuilt (V1.10) | resolves another project's key |
| `tag.go` `ByName` | `tags_name_unique` (V1.10) | latent — only tests call it today |
| `asset.go` `BySHA256` | `assets_sha256_unique` (V1.12) | cross-project asset read; `Download` would presign another project's S3 object |

## Sequencing constraints

1. **`releasesvc.CreatePublish`'s global `max(version)+1` and the `rollbackSQL`/`ByVersion` predicates are one problem, not two.** The unique constraint is already per project; fixing the reads without the allocation, or vice versa, leaves the release table half-migrated. (A separate effort may have already landed the allocation half — check before planning.)
2. **`assetsvc.s3Key()` needs the project prefix and `BySHA256` needs scoping together.** Prefixing the key without scoping the lookup is half a fix: today two projects with identical bytes produce two `assets` rows sharing one S3 object, so deleting one project's asset breaks the other's.
3. **`audit_events.project_id` exists but no Go code can set it.** `repository.AuditEvent` has no such field, so project creation, `AddLocale` and `UpdateLocale` write no audit row at all, and every other audit row is attributed to YouTrip regardless of scope. Add the field first, then the call sites.

## Reconciliation work, not assumption

**`user_project_roles` must be reconciled as a migration step.** `V1.13` backfilled it, and the final fix wave made `usersvc.Grant`/`SetRole` keep it in step — but anything written between those two points, or by any path added since, may have diverged. Treat "reconcile `users.role` into `user_project_roles`" as an explicit migration with verification, not as a safe assumption, before flipping the middleware.

## Gaps the spec itself missed

- **No read route for locales.** The spec listed `POST` and `PATCH` only, so the API can create a dimension it cannot enumerate. Add `GET /projects/{project}/locales` and `GET /projects/{code}` (the latter also makes archived projects reachable, which they currently are not).
- **`merge_requests` never got `UNIQUE (project_id, id)`** although the spec requires it of every parent, so `merge_conflict_resolutions.merge_request_id` and `merge_request_events.merge_request_id` still carry single-column foreign keys. A project-2 resolution can reference project 1's merge request — demonstrated, documented in `scope_workflow_test.go`.

## Bridges to remove

- Every new `project_id` column carries a temporary `DEFAULT 1`. Drop them all, and add an acceptance test asserting no `project_id` column has a default — otherwise a forgotten scope silently becomes YouTrip instead of failing loudly.
- `users.role` is still read by the auth middleware. Switch the lookup, then drop the column.
- The merge advisory lock is still the one-argument global form. Move to `pg_advisory_xact_lock(8675309, project_id)` and add the inverted concurrency test: two projects merging simultaneously must **not** serialize. Mutation-check it by reverting to the one-argument form.
- `V1.11`'s header says the merge "will" filter on `project_id` and derive the lock from it. Make that true, then make it present tense.

## How to write the tests

Five tests in Plan 1 shipped passing for reasons unrelated to their claims, each caught only by an independent adversarial review:

1. An `INSERT` omitting a `NOT NULL` column died at 23502 before the foreign key under test was evaluated.
2. A test re-ran its own inline copy of a migration and asserted on that.
3. A row violated two composite keys, so dropping either left it still rejected — neither was isolated.
4. An assertion counted rows after a code path that never wrote any.
5. A count query targeted a different key than the failing cases wrote.

Two mechanical defences now exist and must be kept: `requireRejected` asserts `pq.Error.Constraint` by name (with `requireRejectedNotNull` and `requireRejectedBySentinel` variants), and `localeID` is project-scoped. Beyond those, answer **"what change would make this fail?"** for every assertion before writing it, and mutation-check each constraint *independently* rather than reverting several at once.

## Environment notes

- `chi v4.1.2`'s `Recoverer`/`PrintPrettyStack` re-panics on a nil-pointer panic's stack trace, turning a nil-service panic into an unrecoverable test-process failure. Use `portalRouterForUser`'s `mutate` hook to install real fakes rather than relying on a nil service to panic.
- Integration tests spin their own PostgreSQL per run via testcontainers, so parallel worktrees do not contend. `.superpowers/` is git-ignored and therefore absent inside a worktree — pass briefs by absolute path.
