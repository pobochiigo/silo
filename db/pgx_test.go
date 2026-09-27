package db

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockPgxTx embeds pgx.Tx to satisfy the interface and overrides what the
// tests exercise.
type mockPgxTx struct {
	pgx.Tx
	committed  bool
	rolledback bool
}

func (m *mockPgxTx) Commit(ctx context.Context) error {
	m.committed = true
	return nil
}

func (m *mockPgxTx) Rollback(ctx context.Context) error {
	m.rolledback = true
	return nil
}

func (m *mockPgxTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), nil
}

// mockPgxPool implements PGXBeginner and PGXCommon.
type mockPgxPool struct {
	begin func(ctx context.Context) (pgx.Tx, error)
}

func (m *mockPgxPool) Begin(ctx context.Context) (pgx.Tx, error) {
	return m.begin(ctx)
}

func (m *mockPgxPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), nil
}

func (m *mockPgxPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

func (m *mockPgxPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return nil
}

func (m *mockPgxPool) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return nil
}

func (m *mockPgxPool) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}

// mockPgxTxPool additionally implements PGXTxBeginner.
type mockPgxTxPool struct {
	mockPgxPool
	gotOpts *pgx.TxOptions
}

func (m *mockPgxTxPool) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	m.gotOpts = &o
	return &mockPgxTx{}, nil
}

func TestPGXTransactorAndExecutor(t *testing.T) {
	ctx := context.Background()
	mockTx := &mockPgxTx{}

	pool := &mockPgxPool{
		begin: func(ctx context.Context) (pgx.Tx, error) {
			return mockTx, nil
		},
	}

	transactor := NewPGXTransactor(pool)

	tx, txCtx, err := transactor.BeginTx(ctx)
	require.NoError(t, err)
	assert.Equal(t, mockTx, tx)

	extractedTx, ok := ExtractPGXTx(txCtx)
	assert.True(t, ok)
	assert.Equal(t, mockTx, extractedTx)

	exec := PGXExecutor(txCtx, pool)
	assert.Equal(t, mockTx, exec)

	require.NoError(t, tx.Commit(txCtx))
	assert.True(t, mockTx.committed)
}

func TestPGXExecutorFallback(t *testing.T) {
	pool := &mockPgxPool{}
	assert.Equal(t, pool, PGXExecutor(context.Background(), pool))
}

func TestPGXExecutor_PanicsWhenSQLTransactionIsActive(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()

	// SQLTransactor opened the transaction, but this repository talks pgx.
	_, txCtx, err := NewSQLTransactor(db).BeginTx(context.Background())
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"db: context carries a database/sql transaction (*sql.Tx) but the repository uses pgx; use Executor/XExecutor or begin transactions with PGXTransactor",
		func() { PGXExecutor(txCtx, &mockPgxPool{}) })
}

func TestPGXTransactor_PassesTxOptions(t *testing.T) {
	pool := &mockPgxTxPool{}
	opts := pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}

	_, txCtx, err := NewPGXTransactor(pool, WithPGXTxOptions(opts)).BeginTx(context.Background())
	require.NoError(t, err)
	require.NotNil(t, pool.gotOpts)
	assert.Equal(t, opts, *pool.gotOpts)

	_, ok := ExtractPGXTx(txCtx)
	assert.True(t, ok)
}

func TestNewPGXTransactor_PanicsWhenOptionsUnsupported(t *testing.T) {
	pool := &mockPgxPool{}

	assert.Panics(t, func() {
		NewPGXTransactor(pool, WithPGXTxOptions(pgx.TxOptions{IsoLevel: pgx.Serializable}))
	}, "a pool without BeginTx cannot honour tx options; fail at startup")
	assert.NotPanics(t, func() { NewPGXTransactor(pool) })
}
