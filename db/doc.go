// Package db adapts database/sql, sqlx and pgx transactions to the uow
// package's Transactor contract and resolves the active transaction from a
// context. Each driver family lives in its own file:
//
//   - sql.go: SQLTransactor, Executor and the context key shared with sqlx
//   - sqlx.go: SQLXTransactor and XExecutor
//   - pgx.go: PGXTransactor and PGXExecutor with their own context key
//   - retry.go: IsRetryableTxError, a retry evaluator for PostgreSQL
//
// The package assumes one database per context: it stores the active
// database/sql or sqlx transaction under a single context key and the active
// pgx transaction under another, and the Executor helpers hand whatever is
// there to any repository that asks. An executor helper that finds a
// transaction of the other driver family in the context panics rather than
// fall back to the pool, because the statement would otherwise run outside
// the transaction the caller believes it is in; repositories must use the
// same driver family as the transactor that opened the transaction.
// Applications that talk to several databases must not nest Unit of Work
// boundaries that belong to different databases.
package db
