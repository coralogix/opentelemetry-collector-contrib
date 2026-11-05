# DBM patch ledger

Receiver patches that Coralogix Database Monitoring (DBM) carries on `feat/dbm` on top of the upstream release this fork is based on. Policy: DBM S2 fork maintenance policy and ADR-0008 (amended by DEV-035) in the DBM program repo.

- **Fork base:** upstream `v0.164.1` (#33).
- **One branch per patch** (`feat/dbm-p<N>-<slug>`), with self-contained commits, so each patch can become its own upstream PR at UPSTREAM-FINAL. This file is never part of a patch branch.
- **Upstream syncs:** merge upstream **release tags** only (see `UPSTREAM_UPDATE.md`), within 5 working days of each contrib release, together with the cdot bump. Never more than 2 minors behind.
- **Per-merge gate** for each patched receiver: unit tests with `-race`, `make generate` + `git diff --exit-code`, `make lint`, and the receiver's `TestIntegrationTopQuery*` tests. Regenerate generated files; never hand-merge them.
- **Each merge of a release** updates the "Last verified" column and the conflict log below.
- **Exit:** a row leaves the fork only after the upstream release that contains it is in cdot (status `upstreamed`).
- **Upstream issue/PR** stays empty until UPSTREAM-FINAL. It is written by a human (DEV-029, `AGENTS.md`).

## Patches

| Id | Receiver | Files | DBM task | Fork branch / PR | Commits (on top of the branch base) | Upstream issue / PR | Status | Last verified | Exit condition |
|---|---|---|---|---|---|---|---|---|---|
| P1 | `receiver/postgresqlreceiver` | `delta.go` (new), `scraper.go`, `factory.go`, `metadata.yaml` (+ generated), `templates/topQueryTemplate.tmpl`, tests | W0-02 (CX-67907) | `feat/dbm-w0-02`, #34 | `bc0d3cb4fb..49421dab7b` (15) | — | carried (in review) | v0.164.1 | upstream release with the delta reset / cold-start fix and the toplevel key, in cdot |
| P2 | `receiver/postgresqlreceiver` | `config.go`, `config.schema.yaml`, `factory.go`, `client.go`, `scraper.go`, `templates/topQueryTemplate.tmpl`, `README.md`, tests, `.chloggen` | EC-02 (CX-68220) | `feat/dbm-p2-cumulative-start-time`, #35 | `49421dab7b..38cd861eee` (3; stacked on P1) | — | carried (in review) | v0.164.1 | upstream release with cumulative top_query + start time + toplevel, in cdot |
| P3 | `receiver/postgresqlreceiver` | `config.go`, `config.schema.yaml`, `factory.go`, `client.go`, `scraper.go`, `templates/topQueryTemplate.tmpl`, `README.md`, tests, `.chloggen` | EC-02 (CX-68220) | `feat/dbm-p3-candidate-order`, #36 | `49421dab7b..e3f8104b30` (3; stacked on P1) | — | carried (in review) | v0.164.1 | upstream release with a top_query candidate order by time, in cdot |
| P4 | `receiver/oracledbreceiver` | — | EC-03 | — | — | — | planned | — | — |
| P5 | `receiver/postgresqlreceiver` | — | EC-04 | — | — | — | planned | — | — |

Not carried in the fork (S2 decision 2): instance totals (preset `sqlquery` sweeps, EC-07); entity emission and `dbmprocessor` (cdot repo, EC-05 / EC-06).

## Known interactions between patches

- **P2 × P3:** both add a parameter to `client.getTopQuery` (and `mockClient`) and a field + check in `config.go`. Whichever merges second resolves by keeping both parameters (`toplevel, statsSince bool, orderBy string`) and adding `"statsSince": false` to `TestTopQueryCandidateOrderSQL`. Trial merge on 2026-11-05 with that resolution: unit `-race` 492 pass, `make lint` 0, `TestIntegrationTopQuery*` pass.

## Conflict log (upstream release merges)

| Date | Upstream tag | Patches | Conflicts | Resolution |
|---|---|---|---|---|
| — | — | — | Expected at the next merge: upstream `fdee279414` (#52548) adjoins P1's `scraper.go` hunk (S2 F3) | — |
