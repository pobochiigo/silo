# uow: one service, three drivers

A small ledger: accounts with a balance and the entries that move money
between them. The service, the unit of work manager and the generated
middlewares are shared; the repository exists three times, once per driver
family Silo supports, and `-driver` picks one at start-up.

```bash
docker compose up -d postgres        # from the examples directory
go run ./uow -driver pgx
go run ./uow -driver sql
go run ./uow -driver sqlx
go run ./uow -driver pgx -v          # Debug logging shows every "<Method> started" line
```

## Layout

| Path | What it is |
|---|---|
| `ledger/ledger.go` | `Account`, `Entry`, the `Repository` and `Service` interfaces with their directives, and the `go:generate` lines. |
| `ledger/service.go` | The business logic, written once against `Repository`. |
| `ledger/*.gen.go` | Generated: `uow_repo`, `logging`, `tracing`, `metrics` for the repository; `uow_service`, `logging`, `tracing` for the service. |
| `ledger/sqlrepo/` | `database/sql` implementation: `db.Executor` resolves `*sql.Tx` or the pool. Pairs with `db.NewSQLTransactor`. |
| `ledger/sqlxrepo/` | `sqlx` implementation: struct scanning and named queries through `db.XExecutor`. Pairs with `db.NewSQLXTransactor`, which injects a genuine `*sqlx.Tx` so named queries keep their `$N` bindvars inside transactions. |
| `ledger/pgxrepo/` | `pgx` implementation: `db.PGXExecutor` resolves `pgx.Tx` or the pool. Pairs with `db.NewPGXTransactor`. |
| `main.go` | Opens the chosen driver with SERIALIZABLE transactions, wires the decorators, and walks through the seven steps below. |

The database/sql and sqlx variants use pgx's `database/sql` driver
(`github.com/jackc/pgx/v5/stdlib`), so all three talk to the same PostgreSQL.

## What the run shows

1. **`OpenAccount` runs in `RunWith`.** `CreateAccount` and `AddEntry` are
   queued while the method runs and committed together afterwards. The
   `*Account` the service returns is the object it passed in: with `-v`, the
   `CreateAccount started` line appears before any transaction is opened.
2. **`Transfer` runs in `RunWith` with the check in the write.** The four
   writes are queued and run in one transaction after the method returns.
   `AdjustBalance` refuses a debit the balance does not cover
   (`... AND balance + $1 >= 0`) and reports `ErrInsufficientFunds`, so the
   unit of work rolls back and nothing is written. The service reads both
   accounts first only for precise not-found errors; it does not compare the
   balance, because that comparison would be stale by the time the debit runs.
3. **Concurrency under SERIALIZABLE.** Eight workers push money back and forth
   between the two accounts. PostgreSQL rejects conflicting transactions with
   `SQLSTATE 40001` (and the occasional `40P01` deadlock); `db.IsRetryableTxError`
   recognises them and the manager re-runs the queued writes of that
   `Transfer`. The run reports how many transactions were retried, typically
   dozens out of 200. The `AdjustBalance failed ... SQLSTATE 40001` lines at
   Error level are those conflicts as the repository's logging middleware
   sees them. The conditional debit itself needs no isolation level; the
   example keeps SERIALIZABLE for `ApplyInterest`.
4. **Invariants.** The total balance is still 20000 and there are exactly two
   entries per committed transfer, counted through the driver's own pool.
5. **`ApplyInterest` runs as one task** because it is marked
   `//middlegen:in-tx`: `BEGIN` first, `ListAccounts` inside the transaction,
   then `AdjustBalance` and `AddEntry` execute at once for every account (the
   entry comes back with its `id`), then `COMMIT`. A serialization failure
   re-runs the whole method.
6. **Nesting.** Calling `ApplyInterest` inside `manager.RunWith` queues it on
   that boundary: the call returns at once, a read made in the action still
   sees the old balance, and the interest lands when the boundary commits.
   Inside `manager.RunInTx` it runs immediately, in the open transaction.
7. **`Statement` is a read.** Nothing is deferred, so `RunWith` opens no
   transaction at all.

A run on sqlx ends like this:

```
== 3. 8 workers x 25 transfers between the two accounts under SERIALIZABLE
   200 transfers committed, 0 refused for insufficient funds, 114 transactions retried after a serialization failure or deadlock, 6.554s

== 4. Invariants: money was only moved, and every transfer left exactly two entries
   alice   11784
   bob      8216
   total balance 20000 (want 20000), entries 402 (want 402)

== 5. ApplyInterest runs as one task (//middlegen:in-tx): BEGIN first, ListAccounts inside the transaction, the writes execute at once, then COMMIT
   alice   11901
   bob      8298

== 6. Nesting: an in-tx method called inside RunWith is queued and runs with that boundary's transaction; inside a task it runs at once
   inside the RunWith action, after the call: alice 11901 (unchanged, the task is queued)
   after the boundary committed:                alice 12020
   inside a RunInTx task: ran at once, alice 12140
```

## Things worth copying

- Repositories never hold a transaction. They call `db.Executor`,
  `db.XExecutor` or `db.PGXExecutor` with the pool as fallback and let the
  context decide.
- `AdjustBalance` puts the check in the write. One `UPDATE` with the
  condition in its `WHERE` clause is atomic under any isolation level, and
  "no row updated" becomes the domain error. The `CHECK (balance >= 0)`
  constraint stays as the safety net.
- `AddEntry` scans `RETURNING id, created_at` into the entry it was given.
  When the call was deferred the caller already holds that pointer, so its
  entry is filled in as soon as the unit of work commits.
- Isolation is a transactor option (`db.WithPGXTxOptions`, `db.WithSQLTxOptions`,
  `db.WithSQLXTxOptions`); the service does not know or care.
- The retry evaluator wraps `db.IsRetryableTxError` only to count; in an
  application, pass `db.IsRetryableTxError` directly.
