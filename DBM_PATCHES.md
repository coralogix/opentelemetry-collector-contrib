# DBM patch ledger

Receiver patches that Coralogix Database Monitoring (DBM) carries on `feat/dbm` on top of the upstream release this fork is based on. Policy: DBM S2 fork maintenance policy and ADR-0008 (amended by DEV-035) in the DBM program repo.

- **Fork base:** upstream `v0.164.1` (#33). `feat/dbm` = that base + P1 (merge `7d3bb20ae6`) + P2 (merge `a7fd327a8f`) + P3 (merge `5eea123dbc`), 2026-11-05; + fork CI cleanup #41 (`4317173470`) + P5 (merge `ac329a0db6`), 2026-11-08.
- **One branch per patch** (`feat/dbm-p<N>-<slug>`), with self-contained commits, so each patch can become its own upstream PR at UPSTREAM-FINAL. This file is never part of a patch branch.
- **Upstream syncs:** merge upstream **release tags** only (see `UPSTREAM_UPDATE.md`), within 5 working days of each contrib release, together with the cdot bump. Never more than 2 minors behind.
- **Per-merge gate** for each patched receiver: unit tests with `-race`, `make generate` + `git diff --exit-code`, `make lint`, and the receiver's `TestIntegrationTopQuery*` tests. Regenerate generated files; never hand-merge them.
- **Each merge of a release** updates the "Last verified" column and the conflict log below.
- **Exit:** a row leaves the fork only after the upstream release that contains it is in cdot (status `upstreamed`).
- **Upstream issue/PR** stays empty until UPSTREAM-FINAL. It is written by a human (DEV-029, `AGENTS.md`).

## Patches

| Id | Receiver | Files | DBM task | Fork branch / PR | Commits (on top of the branch base) | Upstream issue / PR | Status | Last verified | Exit condition |
|---|---|---|---|---|---|---|---|---|---|
| P1 | `receiver/postgresqlreceiver` | `delta.go` (new), `scraper.go`, `factory.go`, `metadata.yaml` (+ generated), `templates/topQueryTemplate.tmpl`, tests | W0-02 (CX-67907) | `feat/dbm-w0-02`, #34 | `bc0d3cb4fb..49421dab7b` (15), merged into `feat/dbm` as `7d3bb20ae6` | — | carried (merged into `feat/dbm` 2026-11-05) | v0.164.1 | upstream release with the delta reset / cold-start fix and the toplevel key, in cdot |
| P2 | `receiver/postgresqlreceiver` | `config.go`, `config.schema.yaml`, `factory.go`, `client.go`, `scraper.go`, `templates/topQueryTemplate.tmpl`, `README.md`, tests, `.chloggen` | EC-02 (CX-68220) | `feat/dbm-p2-cumulative-start-time`, #35 | `7d3bb20ae6..acfc681faa` (5), merged into `feat/dbm` as `a7fd327a8f` | — | carried (merged into `feat/dbm` 2026-11-05) | v0.164.1 | upstream release with cumulative top_query + start time + toplevel, in cdot |
| P3 | `receiver/postgresqlreceiver` | `config.go`, `config.schema.yaml`, `factory.go`, `client.go`, `scraper.go`, `templates/topQueryTemplate.tmpl`, `README.md`, tests, `.chloggen` | EC-02 (CX-68220) | `feat/dbm-p3-candidate-order`, #36 | `a7fd327a8f..23df35c5e1` (4; rebased onto P2), merged into `feat/dbm` as `5eea123dbc` | — | carried (merged into `feat/dbm` 2026-11-05) | v0.164.1 | upstream release with a top_query candidate order by time, in cdot |
| P4 | `receiver/oracledbreceiver` | — | EC-03 | — | — | — | planned | — | — |
| P5 | `receiver/postgresqlreceiver` | `config.go`, `config.schema.yaml`, `client.go`, `scraper.go`, `query_sample_raw_text.go` (new), `README.md`, tests, `.chloggen` | EC-04 (CX-68221) | `feat/dbm-p5-raw-text`, #39 | `3262ab9ef6..d09fc90597` (2), then `d87c1862a9` merging `feat/dbm` (#41) in; merged into `feat/dbm` as `ac329a0db6` | — | carried (merged into `feat/dbm` 2026-11-08) | v0.164.1 | upstream release with an opt-in raw query sample text (name may change, S2 R5), in cdot |
| P6 | `receiver/sqlqueryreceiver` | `logs_receiver.go`, `config.go`, `config.schema.yaml`, `internal/database_sql.go`, `README.md`, tests (`logs_receiver_max_open_conn_test.go`, `internal/database_sql_test.go`, new), `.chloggen` (2) | EC-07 (CX-68224) | `feat/dbm-p6-sqlquery-logs-max-open-conn`, #43 | `dd0955d573..36fb29fb1c` (7) | — (a human files the upstream issue) | open (draft #43, not merged into `feat/dbm`) | v0.164.1 | upstream release where the sqlquery logs receiver honours `max_open_conn` (one shared pool) and has an opt-in `max_idle_conn`, in cdot |

Not carried in the fork (S2 decision 2): instance totals (preset `sqlquery` sweeps, EC-07; only the sqlquery receiver's connection bound is patched, P6); entity emission and `dbmprocessor` (cdot repo, EC-05 / EC-06).

## Contract and design decisions (DEV-045)

- **`postgresql.aggregation_temporality` is a contract marker (P2).** Every `db.server.top_query` record in cumulative mode carries `postgresql.aggregation_temporality = cumulative`. On PostgreSQL 13 a cumulative record has neither `postgresql.start_time` nor `postgresql.toplevel`, so the marker is the only field that tells it apart from an upstream delta record. The receiver sets the marker on every cumulative record, also when it cannot attach `start_time` / `toplevel` to a record. **The ingester ACL and the EC-08 drift guard (layer 3) must check this marker**, not `start_time` / `toplevel`: a record without it is a delta record (not a DBM build); a record with it is cumulative.
- **`postgresql.start_time` is optional on any version.** It is absent on PostgreSQL < 14, and can be absent on any version (for example when `pg_stat_statements_info.stats_reset` is NULL or unreadable). When it is absent, detect resets from decreasing counters. The ingester and EC-08 layer 3 must not reject a record because it lacks `start_time` (or `toplevel`).
- **The P2 attributes are not declared in `metadata.yaml` in the fork.** `postgresql.aggregation_temporality`, `postgresql.start_time` and `postgresql.toplevel` are added to the records after `Emit`, because mdatagen sets every declared event attribute on every record: declaring them would change the delta-mode output and the golden files, and every release merge would carry a generated-code regen. **At UPSTREAM-FINAL, declare them in `metadata.yaml`** (with the upstream design for optional event attributes) as part of the upstream PR.

- **P5 `db.query.text.raw` is off by default and never leaves the collector without a lease.** `dbm.allow_literal_capture: true` adds the un-obfuscated text to `db.server.query_sample` records only (not `db.server.top_query`). The receiver does not redact it. The DBM edge processor `dbmprocessor` (EC-06, cdot) runs the BR-1 credential rule and **deletes the attribute unless a Full capture lease applies**; a pipeline without that processor must keep the option off. The attribute is added after `Emit` (not declared in `metadata.yaml`), like the P2 attributes. **Drift-guard marker (EC-08):** `query_sample_raw_text.go` contains `const dbQueryTextRawAttribute = "db.query.text.raw"`.

- **P6 bounds the sqlquery logs receiver's connections (requester decision C, 2026-11-09; split after review).** Upstream opens one `sql.DB` per logs query, applies `max_open_conn` only to the metrics receiver, and keeps idle connections per pool; under the W0-07 role's `CONNECTION LIMIT 3` that loses sweep records (`pq` 53300). P6 has two parts. (1) Bug fix: with `max_open_conn > 0` (or `max_idle_conn` set), the logs receiver's queries share one `internal.NewPool`, as the metrics receiver. (2) Opt-in: a new `max_idle_conn` (unset = `database/sql` default, `0` = keep none), applied by `internal.NewPool` to both receivers. Without either option, the upstream behaviour is unchanged. **Drift-guard marker (EC-08), canonical:** `receiver.createConnection = pool.DB` in `receiver/sqlqueryreceiver/logs_receiver.go`. **EC-08 hand-off:** after P6 merges into `feat/dbm`, cdot pins `receiver/sqlqueryreceiver` and `internal/sqlquery` to the fork and checks that marker. The DBM sweep receivers must render `max_open_conn: 1` **and** `max_idle_conn: 0`; without `max_idle_conn: 0`, the staggered receivers of EC-07 slice 3 (cdot#228, on hold) still go over the limit.

## Known interactions between patches

- **P2 × P3:** both extend `client.getTopQuery` and add a config field and check. Whichever merges second gets textual conflicts in **4 files**; keep both sides in each. **Applied 2026-11-05** when P3 was rebased onto P2 (`a7fd327a8f` → P3 head `23df35c5e1`). The resolution below was used as written, and every gate passed on the result: unit `-race`, generate, lint, and `TestIntegrationTopQuery*` on PG 13 and 17. A future upstream PR keeps the same split:
  - `client.go`: the interface and the implementation take `toplevel, statsSince bool, order TopQueryCandidateOrder`; the template data carries both `"statsSince"` and `"orderBy"` (`order.column()`).
  - `config.go`: both struct fields (`Temporality`, `CandidateOrder`), both types and both `Validate` checks.
  - `scraper.go`: the `getTopQuery` call passes `cumulative && p.statsSinceSupported, p.config.TopQueryCollection.CandidateOrder`.
  - `scraper_test.go`: `mockClient.getTopQuery` takes both parameters, and P2's `TestTopQueryTemplateRendering` must pass `"orderBy"` too, because the template runs with `missingkey=error`.

  P3's `top_query_candidate_order_test.go` merges without a conflict but does not compile or pass until it is adapted: `TestTopQueryCandidateOrderSQL` adds `"statsSince": false`, and `TestTopQueryCandidateOrderClientMapsColumn` passes `statsSince` (`false`) to `getTopQuery`.

  Trial merge of P2 `52018728cf` and P3 `d808aee5a7` (2026-11-05) with this resolution: unit `-race` and `make lint` pass, and all `TestIntegrationTopQuery*` pass on PostgreSQL 13 and 17. The merge was not pushed.

## Conflict log (upstream release merges)

| Date | Upstream tag | Patches | Conflicts | Resolution |
|---|---|---|---|---|
| — | — | — | Expected at the next merge: upstream `fdee279414` (#52548) adjoins P1's `scraper.go` hunk (S2 F3) | — |
