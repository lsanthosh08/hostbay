---
title: Database Tab Usability - Plan
type: feat
date: 2026-11-19
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Database Tab Usability - Plan

## Goal Capsule

- Objective: someone who opens a database host in Hostbay can browse, filter, sort, export and edit its data, work with several queries and large schemas, and find and rerun past queries, without writing SQL for routine work, and cannot change data by accident.
- Means: extend the existing engine interface with optional capabilities found by type assertion, and keep every statement that touches the database built in Go (KTD1, KTD2).
- Authority: this plan, then `CLAUDE.md` (public repository rules, commit rules), then the existing design notes in the code (KTD23 to KTD26 comments).
- Stop conditions: a feature that needs a new heavyweight dependency, or that would let the page send SQL or identifiers Go does not check, is skipped and listed in the report rather than built differently.
- Execution profile: one branch in this worktree, one commit per feature, no PR; the coordinator merges into `main`.

## Product Contract

### Summary

The database tab works but is bare: a lazy object tree, one Monaco editor, a plain HTML table of results with "Load more", a row-edit dialog and a document-edit dialog. This plan adds what a desktop database client is expected to have, most useful first, reusing what the drivers already return.

### Problem Frame

What the tab does today, checked against the code:

| Area | Today | Gap |
|---|---|---|
| Browse | Click a table or view in the tree; `SELECT * FROM t` runs; 500 rows a page; "Load more" | No sort, no filter, no row count, no refresh button |
| Results | Plain `<table>` with every loaded row in the DOM; NULL in muted italics; cut and binary cells saved to a file | No windowing, fixed column widths, no copy, no value viewer, no export |
| Editor | One Monaco editor; Run runs the selection or everything; Ctrl/Cmd+Enter; Cancel | No query tabs, no elapsed time, no autocomplete, no history, no saved queries |
| Safety | Row edit by catalog key, exactly one row or rollback | Any statement runs without confirmation; no read-only mode |
| Editing | Edit one row in a dialog (SQL, Redis); edit a document whole (MongoDB, Elasticsearch) | No insert, no delete, no preview of the statement |
| Tree | Lazy levels, 200 a page | No filter, no refresh |
| Plans | none | No explain |

### Requirements

- R1. A per-connection read-only switch makes Go refuse every request, row edit, insert, delete and document save that is not judged read-only; where cheap, the SQL session itself is also set read-only on the server.
- R2. Running a request that is not judged read-only asks for confirmation first; the judgement comes from Go, the same one R1 uses.
- R3. A table opened from the tree can be sorted by a column and filtered per column (contains, equals, is NULL, is not NULL) on the server, keeps cursor paging, and shows its row count on request; refresh reruns it.
- R4. The result grid stays responsive with tens of thousands of loaded rows, has resizable columns, copies a cell or a row, and shows a long or JSON value in a viewer.
- R5. Results export to CSV or JSON through Go's save dialog with full values, never values cut for display.
- R6. Each saved host keeps its query history and saved queries on this computer; a query can be put back into the editor from either.
- R7. SQL tables with a key can insert a row and delete a row, each after a review step showing the statement Go will run.
- R8. The editor has several query tabs per connection, shows elapsed time while running, and completes table and column names on SQL engines.
- R9. The object tree can be filtered by name and refreshed.
- R10. PostgreSQL, MySQL, SQLite and rqlite, where a plan is a prefix (EXPLAIN, or EXPLAIN QUERY PLAN), show a query plan for a single statement without running it.
- R11. A MongoDB collection opened from the tree can be filtered and sorted with a JSON filter.

### Scope Boundaries

- Out of scope: explain on SQL Server and Oracle (SHOWPLAN is session state; EXPLAIN PLAN writes PLAN_TABLE in the user's transaction), SQL formatting (no formatter is installed), DDL view (PostgreSQL has no native SHOW CREATE; partial support is not worth a per-engine branch now), rows affected (the SQL path uses `QueryContext`, which drops the count; the write confirmation is the only guard), indexes as a tree level (it would change the tree path shape `schemaNames` and `tablePath` rely on), per-host persisted read-only (it lives in the host form, owned by another session), Redis key-pattern browsing in the tree (the editor runs `SCAN` with `MATCH` already), Elasticsearch browse filter, translations of new strings (other locales fall back to English).
- Per-tab results: query tabs hold their own text; the results area shows the latest run.

## Planning Contract

### Key Technical Decisions

- KTD1. One read-only judgement, in Go. A new optional interface `Classifier { ReadOnly(text string) bool }` is implemented by each engine from the checks it already uses for `Rerunnable` (`rerunnable` for SQL, `redisReadOnly`, `mongoReadOnly`, the Elasticsearch method rule). The page never classifies; it asks Go (`DatabaseReadOnly`) and Go enforces. An engine without a classifier is treated as writing.
- KTD2. Structured browse, never page-supplied SQL. `DatabaseOpenTable` takes a `Browse` (sort column, direction, filters with an operator from a fixed list and a value). Go checks every column name against the table's own catalog columns, quotes it with the dialect, and passes values as parameters through a new internal `query(ctx, text, args)`; `Query` keeps its signature.
- KTD3. Row insert and delete mirror `EditRow`: Go builds the statement from the catalog key and columns, runs it in a transaction, and a delete commits only when exactly one row matched. The review step calls a Go preview that returns the statement text and its values for display; the page never sends SQL back. rqlite has no rollback (`rqlite.go` notes it), so its delete names a unique key and changes at most one row.
- KTD4. Export reruns the shown request in Go (editor text, or a table path plus its Browse rebuilt by the same builder), only when it is read-only, on its own pool connection, never the session, so the grid's cursor and an open transaction survive; it sees committed data only, and streams every row with full values to the file the user picks in Go's dialog. CSV cells starting with `=`, `+`, `-`, `@`, tab or carriage return get a leading apostrophe against formula injection.
- KTD5. History is recorded by Go in `DatabaseQuery` under `database:<hostID>` through `store.AddHistory`, with the connection password scrubbed out. Saved queries live in their own 0600 file through `store.Load`/`store.Save`, so `internal/store` needs no change.
- KTD6. Windowing is hand-rolled (fixed row height, absolute offset, overscan); no virtualization dependency.
- KTD8. Explain runs only text the dialect lexer splits into exactly one statement whose first word is SELECT, WITH, VALUES, TABLE, INSERT, UPDATE, DELETE or MERGE; Go adds the fixed non-executing prefix, and it runs through the same read-only gate as a request.
- KTD7. Autocomplete reuses `schemaNames` (the AI feature's walk, 200 tables at most) through a new `DatabaseSchema` call, registered as a Monaco completion provider that answers only for its own editor model.

### Sequencing

U1 first (R1 and R2 gate every write path that exists; each later write path adds its own gate), then U2 to U9 in value order. Each unit is one commit with its tests; U9's three features are three commits.

### Layout

Query tabs above the editor; the editor toolbar holds Run, Cancel, Explain, History and the read-only switch; the browse bar sits above the grid only for tables opened from the tree; the status line holds row count, elapsed time, Load more, Export and Run again; the tree header holds its filter and Refresh. Confirmations quote the text and focus Cancel.

## Implementation Units

### U1. Read-only judgement, read-only switch, confirm before writing

- Requirements: R1, R2.
- Files: `internal/database/database.go`, `sqlfamily.go`, `redis.go`, `mongodb.go`, `elasticsearch.go`, `postgres.go`, `mysql.go`, `sqlite.go`; `internal/app/database.go`; `src/api.ts` (database block); `src/tabs/database/DatabaseTab.tsx`, `QueryEditor.tsx`; `src/i18n/en.ts`.
- Approach: the SQL write-word list covers DDL, rights and T-SQL batch words (a SQL Server batch needs no semicolons) and any MySQL `/*!` comment is writing; rqlite gets no session hook (its connection is the node's). Add `Classifier` and a `ReadOnlyFunc(e Engine) func(string) bool` helper; `dbSession.readOnly`; `DatabaseSetReadOnly`, `DatabaseReadOnly(dbID, text)`; gate `DatabaseQuery`, `DatabaseEditRow`, `DatabaseSaveDocument`, and the new insert/delete calls. SQL engines get a `setReadOnly` dialect hook run on the session connection (PostgreSQL `SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY`, MySQL `SET SESSION TRANSACTION READ ONLY`, SQLite `PRAGMA query_only`), reapplied when the session reopens. The page asks `DatabaseReadOnly` before Run and confirms when false.
- Test scenarios: each engine's classifier on reads, writes and unknown text; `DatabaseQuery` refused in read-only mode for a write, allowed for a read; row edit and document save refused in read-only mode; read-only session hook applied after a session reconnect (sqlite file test).
- Verification: `go test ./internal/database ./internal/app`.

### U2. Structured browse: sort, filter, count, refresh

- Requirements: R3.
- Files: `internal/database/rowedit.go` (or a new `browse.go`), `sqlfamily.go`, dialect files for `asText`; `internal/app/database.go`; `src/tabs/database/` (new `BrowseBar.tsx`, `DatabaseTab.tsx`); `src/api.ts`; `src/i18n/en.ts`.
- Approach: `Browse{Sort, Desc, Filters[]{Column, Op, Value}}`; columns checked against `Children(path)`; contains is `LOWER(asText(col)) LIKE LOWER(?) ESCAPE '!'` with `!`, `%`, `_` escaped; `DatabaseCountRows(dbID, path, browse)` runs `SELECT COUNT(*)` under `CatalogTimeout`.
- Test scenarios: unknown column refused; identifiers with quotes quoted; values passed as args (sqlite end-to-end: filter value `' OR 1=1 --` matches nothing); LIKE wildcards in the value are literal; sort direction; count with filters.
- Verification: `go test ./internal/database`.

### U3. Status line timing

- Requirements: R8 (elapsed time).
- Files: `src/tabs/database/DatabaseTab.tsx`, `shown.ts`, `shown.test.ts`.
- Test scenarios: duration formatting helper.


### U4. Result grid: windowing, resizable columns, copy, value viewer

- Requirements: R4.
- Files: `src/tabs/database/ResultGrid.tsx`, new `grid.ts` + `grid.test.ts`, new `ValueView.tsx`.
- Test scenarios: window range for scroll offsets at the start, middle and end; row-as-TSV copy text; JSON detection for the viewer.

### U5. Export CSV and JSON

- Requirements: R5.
- Files: new `internal/database/export.go` + test; `internal/app/database.go`; `src/tabs/database/DatabaseTab.tsx`.
- Test scenarios: CSV quoting, NULL as empty, formula prefix, binary as hex, JSON array of objects with nulls; refused for a request that is not read-only.

### U6. History and saved queries

- Requirements: R6.
- Files: `internal/app/database.go`, new `internal/app/databasequeries.go` + test; `src/tabs/database/` new `QueryLibrary.tsx`.
- Test scenarios: run recorded with password scrubbed; saved query add, list, remove; history cleared.

### U7. Insert and delete rows with review

- Requirements: R7.
- Files: `internal/database/rowedit.go`, `rowedit_test.go`, `sqlite_test.go`; `internal/app/database.go`; `ResultGrid.tsx`.
- Approach: the insert and delete calls pass the same read-only gate as row edits.
- Test scenarios: delete of exactly one row commits; zero rows reports deleted meanwhile; insert with NULL and omitted columns; preview text matches the statement run; refused in read-only mode.

### U8. Query tabs and autocomplete

- Requirements: R8.
- Files: `QueryEditor.tsx`, new `tabs.ts` + test, `internal/app/database.go` (`DatabaseSchema`).
- Test scenarios: add, close and rename tab state helpers; completion items built from schema.

### U9. Tree filter and refresh (R9), explain (R10), MongoDB browse filter (R11), each its own commit

- Requirements: R9, R10, R11.
- Files: `ObjectTree.tsx`, `internal/database/*` (`Explainer`), `mongodb.go`.
- Test scenarios: name filter helper (loaded levels only, says so); explain prefix per dialect, `SELECT 1; DELETE FROM t` and `ANALYZE DELETE FROM t` refused; Mongo browse command with filter and sort parsed in Go, bad JSON refused.

## Verification Contract

- `go vet ./...`, `go test ./...` (after `npm run build`), `npx tsc --noEmit`, `npx vitest run`, `npm run build`.
- Bindings regenerate with `wails3 generate bindings -d src/bindings -clean=true -ts -i`; only database-related binding changes are committed.
- No live database: tests use SQLite files, `internal/database/dbtest` fakes, and skipped integration tests.

## Definition of Done

- Every unit built is committed on its own with its tests; skipped features are listed with the reason.
- All verification commands pass; no experimental code left behind.
- New strings exist in `en.ts`; no real hostnames, names or credentials anywhere.

## Appendix

### Review resolution (2026-11-19)

Reviewers: coherence, feasibility, scope-guardian, design-lens, security-lens (cross-model pass skipped: no approval to send the plan to another provider in a non-interactive run).
Applied: T-SQL batch and `/*!` classifier gaps (fixed in code before U1 shipped), explain single-statement rule and engine scope (KTD8, R10), export on a pool connection (KTD4), rqlite hook exclusion, SQL Server `[` escaping, history keyed by host (R6), SQL-only autocomplete (R8), U9 split into three commits, objective widened, layout section, gating of later write paths.
Kept as decided: query tabs share one results area, labelled by the request that produced them; rows affected stays out of scope.
