package db

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/pobochiigo/silo/uow"
)

// SQLCommon defines the common execution contract shared by *sql.DB and *sql.Tx.
type SQLCommon interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLXCommon defines the common execution contract shared by *sqlx.DB and *sqlx.Tx.
type SQLXCommon interface {
	SQLCommon
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
}

type txKey struct{}

// InjectTx injects the transactional executor into the context.
func InjectTx(ctx context.Context, tx SQLCommon) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// ExtractTx extracts the transactional executor from the context.
func ExtractTx(ctx context.Context) (SQLCommon, bool) {
	tx, ok := ctx.Value(txKey{}).(SQLCommon)
	return tx, ok
}

// Executor resolves the active transactional executor in the context if present, or falls back to the provided default.
func Executor(ctx context.Context, fallback SQLCommon) SQLCommon {
	if tx, ok := ExtractTx(ctx); ok {
		return tx
	}
	return fallback
}

// XExecutor resolves the active transactional executor in the context if present.
// If the active executor in the context is a standard library *sql.Tx transaction,
// it dynamically wraps it in a type-safe *sqlx.Tx wrapper inheriting name mapping
// from the fallback connection pool at runtime.
func XExecutor(ctx context.Context, fallback SQLXCommon) SQLXCommon {
	tx, ok := ExtractTx(ctx)
	if !ok {
		return fallback
	}

	if stdTx, ok := tx.(*sql.Tx); ok {
		if sqlxDB, ok := fallback.(*sqlx.DB); ok {
			return &sqlx.Tx{
				Tx:     stdTx,
				Mapper: sqlxDB.Mapper,
			}
		}
		return &sqlx.Tx{
			Tx: stdTx,
		}
	}

	if sqlxTx, ok := tx.(SQLXCommon); ok {
		return sqlxTx
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

// SQLTransactor adapts database/sql transaction lifecycle to uow.Transactor.
type SQLTransactor struct {
	db interface {
		BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	}
}

// NewSQLTransactor creates a new SQLTransactor.
func NewSQLTransactor(db interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}) *SQLTransactor {
	return &SQLTransactor{db: db}
}

// BeginTx begins a transaction and injects it into context.
func (t *SQLTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	txCtx := InjectTx(ctx, tx)
	return &stdTxAdapter{tx: tx}, txCtx, nil
}

// PGXCommon defines the common execution contract shared by pgx.Tx and pgxpool.Pool.
type PGXCommon interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type pgxTxKey struct{}

// InjectPGXTx injects a native pgx transaction into the context.
func InjectPGXTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, pgxTxKey{}, tx)
}

// ExtractPGXTx extracts the native pgx transaction from the context.
func ExtractPGXTx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(pgxTxKey{}).(pgx.Tx)
	return tx, ok
}

// PGXExecutor resolves the active pgx transaction in the context if present, or falls back to the provided default.
func PGXExecutor(ctx context.Context, fallback PGXCommon) PGXCommon {
	if tx, ok := ExtractPGXTx(ctx); ok {
		return tx
	}
	return fallback
}

// PGXTransactor adapts pgxpool.Pool transaction lifecycle to uow.Transactor.
type PGXTransactor struct {
	pool interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	}
}

// NewPGXTransactor creates a new PGXTransactor.
func NewPGXTransactor(pool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}) *PGXTransactor {
	return &PGXTransactor{pool: pool}
}

// BeginTx begins a transaction and injects it into context.
func (t *PGXTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	txCtx := InjectPGXTx(ctx, tx)
	return tx, txCtx, nil
}
