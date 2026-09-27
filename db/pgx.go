package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pobochiigo/silo/uow"
)

// PGXCommon defines the execution contract shared by *pgxpool.Pool, *pgx.Conn
// and pgx.Tx, so a repository can run single statements, batches and COPY
// through whichever of them the context resolves to.
type PGXCommon interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
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

// PGXExecutor resolves the active pgx transaction in the context, or falls
// back to the provided pool when there is none.
//
// PGXExecutor panics when the context carries a database/sql or sqlx
// transaction instead: the repository and the transactor disagree on the
// driver, and running the statement on the pool would silently put it outside
// the transaction. Code that deliberately targets another database from
// inside a unit of work must use its own pool directly.
func PGXExecutor(ctx context.Context, fallback PGXCommon) PGXCommon {
	if tx, ok := ExtractPGXTx(ctx); ok {
		return tx
	}
	if tx, ok := ExtractTx(ctx); ok {
		panic(fmt.Sprintf("db: context carries a database/sql transaction (%T) but the repository uses pgx; use Executor/XExecutor or begin transactions with PGXTransactor", tx))
	}
	return fallback
}

// PGXBeginner is the subset of *pgxpool.Pool needed to begin transactions.
type PGXBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PGXTxBeginner is implemented by pools that support pgx.TxOptions
// (e.g. *pgxpool.Pool); it is required when using WithPGXTxOptions.
type PGXTxBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// PGXTransactor adapts pgxpool.Pool transaction lifecycle to uow.Transactor.
type PGXTransactor struct {
	pool PGXBeginner
	opts *pgx.TxOptions
}

// PGXTransactorOption configures a PGXTransactor.
type PGXTransactorOption func(*PGXTransactor)

// WithPGXTxOptions sets the pgx.TxOptions (isolation level, access mode) used
// for every transaction the transactor begins. The pool passed to
// NewPGXTransactor must also implement PGXTxBeginner; NewPGXTransactor
// panics otherwise so the misconfiguration surfaces at startup.
func WithPGXTxOptions(opts pgx.TxOptions) PGXTransactorOption {
	return func(t *PGXTransactor) {
		t.opts = &opts
	}
}

// NewPGXTransactor creates a new PGXTransactor. It panics when
// WithPGXTxOptions is used with a pool that does not implement PGXTxBeginner,
// because every transaction the transactor begins would fail.
func NewPGXTransactor(pool PGXBeginner, opts ...PGXTransactorOption) *PGXTransactor {
	t := &PGXTransactor{pool: pool}
	for _, opt := range opts {
		opt(t)
	}
	if t.opts != nil {
		if _, ok := t.pool.(PGXTxBeginner); !ok {
			panic(fmt.Sprintf("db: pool %T does not support pgx.TxOptions (missing BeginTx); drop WithPGXTxOptions or use *pgxpool.Pool", pool))
		}
	}
	return t
}

// BeginTx begins a transaction and injects it into context.
func (t *PGXTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	var tx pgx.Tx
	var err error
	if t.opts != nil {
		beginner, ok := t.pool.(PGXTxBeginner)
		if !ok {
			return nil, nil, fmt.Errorf("db: pool %T does not support pgx.TxOptions (missing BeginTx)", t.pool)
		}
		tx, err = beginner.BeginTx(ctx, *t.opts)
	} else {
		tx, err = t.pool.Begin(ctx)
	}
	if err != nil {
		return nil, nil, err
	}
	txCtx := InjectPGXTx(ctx, tx)
	return tx, txCtx, nil
}
