package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/reflectx"

	"github.com/pobochiigo/silo/uow"
)

// SQLXCommon defines the execution contract shared by *sqlx.DB and *sqlx.Tx:
// the database/sql methods plus sqlx's struct scanning, named execution and
// bindvar helpers. It satisfies sqlx.ExtContext, so the package-level helpers
// that take an executor work on it too, for example
// sqlx.NamedQueryContext(ctx, db.XExecutor(ctx, pool), query, arg).
type SQLXCommon interface {
	SQLCommon
	DriverName() string
	Rebind(query string) string
	BindNamed(query string, arg any) (string, []any, error)
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
	QueryxContext(ctx context.Context, query string, args ...any) (*sqlx.Rows, error)
	QueryRowxContext(ctx context.Context, query string, args ...any) *sqlx.Row
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
	PreparexContext(ctx context.Context, query string) (*sqlx.Stmt, error)
}

var (
	_ SQLXCommon      = (*sqlx.DB)(nil)
	_ SQLXCommon      = (*sqlx.Tx)(nil)
	_ sqlx.ExtContext = SQLXCommon(nil)
)

// XExecutor resolves the active transactional executor in the context if present.
// If the active executor in the context is a standard library *sql.Tx transaction,
// it dynamically wraps it in a type-safe *sqlx.Tx wrapper inheriting name mapping
// from the fallback connection pool when that pool is a *sqlx.DB, and sqlx's
// default mapper otherwise.
//
// Limitation of the dynamic *sql.Tx wrap: the constructed *sqlx.Tx has no driver
// name, so bindvar-dependent helpers (NamedExecContext, Rebind, BindNamed) fall
// back to '?' placeholders, which fails on drivers like Postgres that expect $N.
// If repositories use named queries inside transactions, begin transactions with
// SQLXTransactor so the genuine *sqlx.Tx is injected instead.
//
// XExecutor panics when the context carries an executor it cannot adapt
// (neither a *sql.Tx nor an SQLXCommon) or a pgx transaction. Falling back to
// the pool in those cases would silently run the caller's statements outside
// the active transaction.
func XExecutor(ctx context.Context, fallback SQLXCommon) SQLXCommon {
	tx, ok := ExtractTx(ctx)
	if !ok {
		if pgxTx, ok := ExtractPGXTx(ctx); ok {
			panic(fmt.Sprintf("db: context carries a pgx transaction (%T) but the repository uses sqlx; use PGXExecutor or begin transactions with SQLXTransactor", pgxTx))
		}
		return fallback
	}

	if stdTx, ok := tx.(*sql.Tx); ok {
		mapper := reflectx.NewMapperFunc("db", sqlx.NameMapper)
		if sqlxDB, ok := fallback.(*sqlx.DB); ok && sqlxDB.Mapper != nil {
			mapper = sqlxDB.Mapper
		}
		return &sqlx.Tx{
			Tx:     stdTx,
			Mapper: mapper,
		}
	}

	if sqlxTx, ok := tx.(SQLXCommon); ok {
		return sqlxTx
	}

	panic(fmt.Sprintf("db: executor %T found in context cannot be used with sqlx; begin transactions with SQLTransactor or SQLXTransactor", tx))
}

// SQLXTransactor adapts sqlx transaction lifecycle to uow.Transactor.
// Unlike SQLTransactor, it injects a genuine *sqlx.Tx into the context, so
// XExecutor resolves an executor with the driver's bindvar type and name
// mapping intact, which NamedExecContext and friends need inside tasks.
type SQLXTransactor struct {
	db   *sqlx.DB
	opts *sql.TxOptions
}

// SQLXTransactorOption configures a SQLXTransactor.
type SQLXTransactorOption func(*SQLXTransactor)

// WithSQLXTxOptions sets the sql.TxOptions (isolation level, read-only) used
// for every transaction the transactor begins.
func WithSQLXTxOptions(opts *sql.TxOptions) SQLXTransactorOption {
	return func(t *SQLXTransactor) {
		t.opts = opts
	}
}

// NewSQLXTransactor creates a new SQLXTransactor.
func NewSQLXTransactor(db *sqlx.DB, opts ...SQLXTransactorOption) *SQLXTransactor {
	t := &SQLXTransactor{db: db}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// BeginTx begins a transaction and injects the *sqlx.Tx into context.
func (t *SQLXTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	tx, err := t.db.BeginTxx(ctx, t.opts)
	if err != nil {
		return nil, nil, err
	}
	txCtx := InjectTx(ctx, tx)
	return &stdTxAdapter{tx: tx.Tx}, txCtx, nil
}
