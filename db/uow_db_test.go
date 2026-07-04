package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock standard database BeginTx
type mockSQLDB struct {
	beginTx func(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func (m *mockSQLDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return m.beginTx(ctx, opts)
}

// Mock pgx.Tx
type mockPgxTx struct {
	pgx.Tx     // Embed to satisfy interface
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

// Mock pgx pool / connection
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

func TestSQLTransactorAndExecutor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectCommit()

	transactor := NewSQLTransactor(db)
	ctx := context.Background()

	// 1. Begin transaction
	tx, txCtx, err := transactor.BeginTx(ctx)
	require.NoError(t, err)
	assert.NotNil(t, tx)

	// 2. Extract transaction & check Executor
	stdTx, ok := ExtractTx(txCtx)
	assert.True(t, ok)
	assert.NotNil(t, stdTx)

	exec := Executor(txCtx, db)
	assert.Equal(t, stdTx, exec)

	// 3. Commit
	err = tx.Commit(txCtx)
	assert.NoError(t, err)

	assert.NoError(t, mock.ExpectationsWereMet())
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

	// 1. Begin transaction
	tx, txCtx, err := transactor.BeginTx(ctx)
	require.NoError(t, err)
	assert.Equal(t, mockTx, tx)

	// 2. Extract transaction & check PGXExecutor
	extractedTx, ok := ExtractPGXTx(txCtx)
	assert.True(t, ok)
	assert.Equal(t, mockTx, extractedTx)

	exec := PGXExecutor(txCtx, pool)
	assert.Equal(t, mockTx, exec)

	// 3. Commit via uow.Tx interface
	err = tx.Commit(txCtx)
	assert.NoError(t, err)
	assert.True(t, mockTx.committed)
}

func TestPGXExecutorFallback(t *testing.T) {
	ctx := context.Background()
	pool := &mockPgxPool{}

	// When no transaction is present in context, PGXExecutor must return fallback
	exec := PGXExecutor(ctx, pool)
	assert.Equal(t, pool, exec)
}
