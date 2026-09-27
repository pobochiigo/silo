# Repository review: v0.1.0

Second full review of `github.com/pobochiigo/silo`, at the v0.1.0 tag
(`main` at 136a5f7, 2026-09-27), with particular attention to the `uow` and
`db` packages: how a transaction travels through the context, and the split
between the database/sql, sqlx and pgx adapters. The first review is in
[repository-review.md](repository-review.md).

Every finding marked **verified** was reproduced with a test written against
the code as it was, and is fixed on this branch. Where a fix changes
behaviour, the change is listed under *Compatibility*.

## Summary

The transaction travels correctly on the common paths: `RunWith` opens the
transaction after the action, hands the deferred tasks a context carrying the
transaction and no unit of work, and the `db` executors resolve that
transaction for the same driver family. The gaps were at the boundaries
between paths, where the context said one thing and the code assumed another.

| # | Severity | Finding | Status |
|---|---|---|---|
| H1 | High | `RunInTx` nested in a `RunWith` action ran its action with no transaction at all. | Fixed: returns `uow.ErrNoTransaction`. |
| H2 | High | `RunWith`/`RunInTx` called with a context carrying an open transaction but no unit of work (a deferred task) opened a second transaction inside the first. | Fixed: joins the open transaction. |
| M1 | Medium | A task that deferred more work, or a late `Defer` from a goroutine, lost that work silently. | Fixed: `uow.ErrLateDefer`. |
| M2 | Medium | Inside `RunInTx` the generated `uow_repo` middleware still queued writes: callers got echoed inputs instead of results and later reads did not see the writes. | Fixed: writes execute immediately inside an open transaction. |
| M3 | Medium | `uow_service` could only wrap methods in `RunWith`; read-modify-write service methods had no way to get `RunInTx`. | Fixed: `//middlegen:in-tx`. |
| M4 | Medium | An executor helper called with a transaction of the other driver family in the context fell back to the pool, running the statement outside the transaction. | Fixed: panics with a message naming both sides. |
| L1 | Low | `db.PGXCommon` had no `SendBatch`/`CopyFrom`, so batches and COPY could not run through the unit of work. | Fixed. |
| L2 | Low | `db.SQLXCommon` exposed three sqlx methods; the sqlx query helpers were unreachable through `XExecutor`. | Fixed: the full shared surface, satisfying `sqlx.ExtContext`. |
| L3 | Low | The three driver families shared one 300-line file. | Fixed: `sql.go`, `sqlx.go`, `pgx.go`, `retry.go`, `doc.go`; tests split the same way. |
| L4 | Low | The go-kit `TracingMiddleware` set the span status to `Ok` on success while the generated tracing middleware leaves it unset. | Fixed: both leave it unset, as the OpenTelemetry specification asks of instrumentation. |

No new findings in `telemetry` beyond L4, nor in `connectrpc`, `endpoint`,
`middleware` or the generator's type handling; those were reworked in the
first review and re-read here.

## The unit of work and the context

The manager and the `db` adapters communicate through the context alone:

- `uow.Inject`/`uow.Extract` carry the **unit of work**, the task queue.
- `db.InjectTx`/`db.ExtractTx` carry a database/sql or sqlx **transaction**;
  `db.InjectPGXTx`/`db.ExtractPGXTx` carry a pgx one. The transactors put the
  transaction into the context they return from `BeginTx`; the executor
  helpers take it out.
- New in this review: the manager marks the context it hands to tasks and to
  `RunInTx` actions, and `uow.InTransaction` reads the mark.

Which of these the context carries decides what a nested call must do.
Before this review only the unit of work was consulted, which produced H1,
H2 and M2:

| Context carries | Where that happens | Before | Now |
|---|---|---|---|
| unit, no transaction | a `RunWith` action | `RunInTx` joined and ran with no transaction (H1) | `RunInTx` returns `ErrNoTransaction`; `RunWith` joins |
| unit and transaction | a `RunInTx` action | join; generated writes were still queued (M2) | join; generated writes execute immediately |
| transaction, no unit | a deferred task | `RunWith`/`RunInTx` began a second transaction (H2) | run the action in the open transaction with a fresh unit; the outer boundary commits |
| neither | top level | open a boundary | unchanged |

### H1. `RunInTx` inside a `RunWith` action ran without a transaction (verified)

`RunInTx` began with `if _, ok := Extract(ctx); ok { return action(ctx) }`.
Inside a `RunWith` action the context has a unit of work but the transaction
does not exist yet, so the "transactional" action ran on the pool with no
isolation and no rollback, and returned nil. A service method decorated with
`uow_service` (always `RunWith`) that called `manager.RunInTx` for a critical
section got none of what it asked for and no signal.

**Fix.** `RunInTx` tells the two cases apart with `InTransaction`: inside an
open transaction it joins, inside a `RunWith` action it returns
`uow.ErrNoTransaction`. The alternative, opening an independent transaction,
would commit the inner writes even when the outer action later fails, which
is the one thing a unit of work must never do silently. The error names the
way out: make the outer boundary `RunInTx`, which `//middlegen:in-tx` now
provides for generated services (M3).
Test: `TestRunInTx_InsideRunWithActionIsRefused`.

### H2. A boundary started from a deferred task opened a second transaction (verified)

Tasks run with a context carrying the transaction and, deliberately, no unit
of work, so that repository calls made from a task execute immediately.
`RunWith` and `RunInTx` saw no unit and started a new boundary:
`transactor.BeginTx(txCtx)` ran while the first transaction was open. With
`pgxpool` that takes a second connection; the inner transaction then blocks
on any row the outer one has locked while the outer waits for the inner to
return, a deadlock PostgreSQL cannot detect because one side waits in the
application. The probe counted two `BeginTx` calls for one unit of work.

**Fix.** `inTransaction` marks the transactional context. `RunWith` and
`RunInTx` called with a marked context and no unit run the action inside the
open transaction with a fresh unit, run the tasks that unit collected, and
leave the commit to the boundary that opened the transaction.
Tests: `TestRunWith_InsideTaskJoinsTheOpenTransaction`,
`TestRunInTx_InsideTaskJoinsTheOpenTransaction`,
`TestRunWith_JoinedActionErrorPropagates`.

### M1. Late `Defer` calls were dropped silently (verified)

`runTasks` iterated a snapshot taken before execution. A task that deferred
more work, or a goroutine that outlived the action and deferred late,
appended to the queue and nothing ran it; `RunWith` returned nil.

**Fix.** After the snapshot has run, a longer queue is reported as
`uow.ErrLateDefer`. Running the extra tasks instead was rejected: on a retry
the snapshot re-runs and re-defers, so the queue would grow with duplicates.
Tests: `TestRunWith_TaskDeferringTaskIsReported`,
`TestRunInTx_TaskDeferringTaskIsReported`, `TestRunWith_LateDeferIsNotRetried`.

### M2. Generated repositories queued writes inside `RunInTx`

The `uow_repo` template deferred every write when the context carried a unit
of work, including inside `RunInTx`, where the transaction is already open.
The caller received the echoed input instead of the implementation's result
(no `RETURNING` values), and a read after a write in the same action did not
see the write. Deferral exists to avoid holding a transaction open during
the action; once it is open, deferral only loses information.

**Fix.** The template checks `uow.InTransaction(ctx)` and calls the
implementation directly when it is true. Deferred tasks run with the same
mark, so a repository call from a task also executes immediately instead of
re-queueing. `RunInTx` still runs tasks deferred by hand after the action.
Tests: `TestInsideOpenTransactionWritesRunImmediately` in the scaffolded
module, and the golden files.

### M3. No `RunInTx` for generated services

`uow_service` wrapped every method in `RunWith`. `//middlegen:in-tx` on a
method now wraps it in `RunInTx`. The directive is documented in the README
and exercised by the golden files, the scaffolded module and the examples.

### M4. Cross-driver executor mismatch ran statements outside the transaction

`db.Executor` returned the pool when the context held a pgx transaction, and
`db.PGXExecutor` returned the pool when it held a database/sql one: a
repository built for one driver family under a transactor of the other
silently escaped the transaction. `XExecutor` already panicked for an
executor it could not adapt; the other two now do the same for the other
family, with a message naming the repository's and the transactor's driver.
The one legitimate reason to meet the other family's transaction, a
statement aimed at a different database from inside a unit of work, is served
by using that database's pool directly.
Tests: `TestExecutor_PanicsWhenPGXTransactionIsActive`,
`TestXExecutor_PanicsWhenPGXTransactionIsActive`,
`TestPGXExecutor_PanicsWhenSQLTransactionIsActive`.

### L1, L2. Executor contracts

`PGXCommon` gains `SendBatch` and `CopyFrom`, both implemented by
`*pgxpool.Pool`, `*pgx.Conn` and `pgx.Tx`. `SQLXCommon` gains `DriverName`,
`Rebind`, `BindNamed`, `QueryxContext`, `QueryRowxContext` and
`PreparexContext`, the methods `*sqlx.DB` and `*sqlx.Tx` share, and is
asserted at compile time to satisfy `sqlx.ExtContext`, so package-level
helpers such as `sqlx.NamedQueryContext(ctx, db.XExecutor(ctx, pool), ...)`
work; sqlx has no `NamedQueryContext` method on transactions.

### L3. Split by driver family

`db/uow_db.go` became `sql.go` (contract, context key, `Executor`,
`SQLTransactor`), `sqlx.go` (`SQLXCommon`, `XExecutor`, `SQLXTransactor`),
`pgx.go` (its own context key, `PGXExecutor`, `PGXTransactor`), `retry.go`
(`IsRetryableTxError`) and `doc.go` (package documentation, including the
one-database assumption and the guards); the tests follow the same layout.
The database/sql and sqlx transactors keep sharing one context key on
purpose: a plain database/sql repository works inside a transaction begun by
`SQLXTransactor` and vice versa.

## Checked and left as is

- **`stdTxAdapter.Commit` ignores its context.** `*sql.Tx` has no
  context-taking `Commit`; the transaction is bound to the context passed to
  `BeginTx`, and cancelling that context rolls back. Documented on
  `SQLTransactor.BeginTx`.
- **`XExecutor`'s dynamic `*sqlx.Tx` has no driver name.** sqlx offers no way
  to set it, so named queries under `SQLTransactor` keep failing on
  PostgreSQL; the README and the `uow` example steer sqlx users to
  `SQLXTransactor`.
- **`IsRetryableTxError` recognises 40001 and 40P01 only.** Deliberate; the
  ledger example shows both being retried under load.
- **Rollback after a failed commit.** pgx reports `ErrTxClosed`, database/sql
  `ErrTxDone`; both are ignored on purpose.
- **Telemetry.** Resource detector order (SDK, host, environment, then
  configuration) is as documented; the OpenTelemetry error handler writes to
  its own stderr logger, so exporter failures cannot loop through the slog
  bridge; the shared gRPC connection is closed by the composite shutdown.
- **connectrpc.** Endpoint errors pass through untouched, decode and encode
  failures get their codes; the example confirms both paths over the wire.

## Decision left to the maintainer

**Subpackages per driver family.** Package `db` imports database/sql, sqlx
and pgx, so an application using only pgx still links sqlx, and one using
only database/sql links pgx. Splitting into `db/sqldb`, `db/sqlxdb` and
`db/pgxdb` would remove that, at the cost of changing every import path and
constructor name. The file split in L3 is the first step either way; the
package split is a public API decision and was not made here. If it is made,
the shared database/sql and sqlx context key must stay shared.

## Compatibility

These changes call for a minor version bump (v0.2.0):

- `RunInTx` inside a `RunWith` action returns `ErrNoTransaction` instead of
  running unprotected.
- `RunWith`/`RunInTx` inside a deferred task join the open transaction
  instead of opening a second one.
- Late `Defer` calls return `ErrLateDefer` instead of being dropped.
- Regenerated `uow_repo` middlewares execute writes immediately inside an
  open transaction; the templates changed, so regenerate with the new
  generator.
- `db.Executor`, `db.XExecutor` and `db.PGXExecutor` panic on a transaction of
  the other driver family.
- `db.PGXCommon` and `db.SQLXCommon` gained methods; custom implementations
  such as test fakes must add them.
- The go-kit `TracingMiddleware` no longer sets `codes.Ok` on success.

## Verification

- `go test -race ./...` on the library, including the scaffolded module that
  compiles and runs the generated code.
- `gofmt`, `go vet`, `staticcheck 2026.2.1` and `go mod tidy` clean.
- The `examples` module builds, its generated files match the templates, and
  every example ran: the ledger on pgx, database/sql and sqlx against
  PostgreSQL 16 with 200 concurrent transfers each, dozens of serialization
  failures and deadlocks retried, and the balance and entry invariants held;
  the zero-infrastructure generator tour; trace propagation over HTTP and
  gRPC metadata; Connect error mapping over the wire.
