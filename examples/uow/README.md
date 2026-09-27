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
| `main.go` | Opens the chosen driver with SERIALIZABLE transactions, wires the decorators, and walks through the six steps below. |

The database/sql and sqlx variants use pgx's `database/sql` driver
(`github.com/jackc/pgx/v5/stdlib`), so all three talk to the same PostgreSQL.

## What the run shows

1. **`OpenAccount` runs in `RunWith`.** `CreateAccount` and `AddEntry` are
   queued while the method runs and committed together afterwards. The
   `*Account` the service returns is the object it passed in: with `-v`, the
   `CreateAccount started` line appears before any transaction is opened.
2. **`Transfer` runs in `RunInTx`** because it is marked `//middlegen:in-tx`.
   The balance check and the four writes share one transaction, so a transfer
   that exceeds the balance fails before anything is written and the balances
   are unchanged.
3. **Concurrency under SERIALIZABLE.** Eight workers push money back and forth
   between the two accounts. PostgreSQL rejects conflicting transactions with
   `SQLSTATE 40001` (and the occasional `40P01` deadlock); `db.IsRetryableTxError`
   recognises them and the manager re-runs the whole `Transfer`. The run
   reports how many transactions were retried, typically dozens out of 200.
   The `AdjustBalance failed ... SQLSTATE 40001` lines at Error level are
   those conflicts as the repository's logging middleware sees them.
4. **Invariants.** The total balance is still 20000 and there are exactly two
   entries per committed transfer, counted through the driver's own pool.
5. **Nesting.** Calling `Transfer` inside `manager.RunWith` returns
   `uow.ErrNoTransaction`: a `RunInTx` method cannot join a boundary whose
   transaction is not open yet. Inside `manager.RunInTx` it joins, and both
   transfers commit with the outer transaction.
6. **`Statement` is a read.** Nothing is deferred, so `RunWith` opens no
   transaction at all.

A run on pgx ends like this:

```
== 3. 8 workers x 25 transfers between the two accounts under SERIALIZABLE
   200 transfers committed, 0 refused for insufficient funds, 71 transactions retried after a serialization failure or deadlock, 8.376s

== 4. Invariants: money was only moved, and every transfer left exactly two entries
   alice    8616
   bob     11384
   total balance 20000 (want 20000), entries 402 (want 402)

== 5. Nesting: RunInTx refuses to join a RunWith action, but joins an outer RunInTx
   inside RunWith: uow: RunInTx called inside a RunWith boundary whose transaction is not open yet
   inside RunInTx: <nil>
```

## Things worth copying

- Repositories never hold a transaction. They call `db.Executor`,
  `db.XExecutor` or `db.PGXExecutor` with the pool as fallback and let the
  context decide.
- `AddEntry` scans `RETURNING id, created_at` into the entry it was given.
  When the call was deferred the caller already holds that pointer, so its
  entry is filled in as soon as the unit of work commits.
- Isolation is a transactor option (`db.WithPGXTxOptions`, `db.WithSQLTxOptions`,
  `db.WithSQLXTxOptions`); the service does not know or care.
- The retry evaluator wraps `db.IsRetryableTxError` only to count; in an
  application, pass `db.IsRetryableTxError` directly.
