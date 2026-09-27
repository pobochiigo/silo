package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pobochiigo/silo/uow"
)

// SQLCommon defines the common execution contract shared by *sql.DB and *sql.Tx.
type SQLCommon interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txKey stores the active database/sql or sqlx transaction. Both transactor
// families share it so a repository written against database/sql works in a
// transaction begun by SQLXTransactor and vice versa.
type txKey struct{}

// InjectTx injects the transactional executor into the context. A context
// holds at most one executor; injecting a second one replaces the first.
func InjectTx(ctx context.Context, tx SQLCommon) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// ExtractTx extracts the transactional executor from the context.
func ExtractTx(ctx context.Context) (SQLCommon, bool) {
	tx, ok := ctx.Value(txKey{}).(SQLCommon)
	return tx, ok
}

// Executor resolves the active database/sql (or sqlx) transaction in the
// context, or falls back to the provided pool when there is none.
//
// Executor panics when the context carries a pgx transaction instead: the
// repository and the transactor disagree on the driver, and running the
// statement on the pool would silently put it outside the transaction. Code
// that deliberately targets another database from inside a unit of work
// must use its own pool directly rather than through Executor.
func Executor(ctx context.Context, fallback SQLCommon) SQLCommon {
	if tx, ok := ExtractTx(ctx); ok {
		return tx
	}
	if tx, ok := ExtractPGXTx(ctx); ok {
		panic(fmt.Sprintf("db: context carries a pgx transaction (%T) but the repository uses database/sql; use PGXExecutor or begin transactions with SQLTransactor", tx))
	}
	return fallback
}

// stdTxAdapter wraps *sql.Tx to satisfy uow.Tx.
type stdTxAdapter struct {
	tx *sql.Tx
}

func (a *stdTxAdapter) Commit(ctx context.Context) error {
	return a.tx.Commit()
}

func (a *stdTxAdapter) Rollback(ctx context.Context) error {
	return a.tx.Rollback()
}

// SQLBeginner is the subset of *sql.DB needed to begin transactions.
type SQLBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// SQLTransactor adapts database/sql transaction lifecycle to uow.Transactor.
type SQLTransactor struct {
	db   SQLBeginner
	opts *sql.TxOptions
}

// SQLTransactorOption configures a SQLTransactor.
type SQLTransactorOption func(*SQLTransactor)

// WithSQLTxOptions sets the sql.TxOptions (isolation level, read-only) used
// for every transaction the transactor begins.
func WithSQLTxOptions(opts *sql.TxOptions) SQLTransactorOption {
	return func(t *SQLTransactor) {
		t.opts = opts
	}
}

// NewSQLTransactor creates a new SQLTransactor.
func NewSQLTransactor(db SQLBeginner, opts ...SQLTransactorOption) *SQLTransactor {
	t := &SQLTransactor{db: db}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// BeginTx begins a transaction and injects it into context. The transaction
// is bound to ctx as database/sql does: cancelling ctx rolls it back.
func (t *SQLTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	tx, err := t.db.BeginTx(ctx, t.opts)
	if err != nil {
		return nil, nil, err
	}
	txCtx := InjectTx(ctx, tx)
	return &stdTxAdapter{tx: tx}, txCtx, nil
}
