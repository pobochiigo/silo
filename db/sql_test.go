package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSQLDB records the options passed to BeginTx.
type mockSQLDB struct {
	beginTx func(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func (m *mockSQLDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return m.beginTx(ctx, opts)
}

func TestSQLTransactorAndExecutor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectCommit()

	transactor := NewSQLTransactor(db)
	ctx := context.Background()

	tx, txCtx, err := transactor.BeginTx(ctx)
	require.NoError(t, err)
	assert.NotNil(t, tx)

	stdTx, ok := ExtractTx(txCtx)
	assert.True(t, ok)
	assert.NotNil(t, stdTx)

	exec := Executor(txCtx, db)
	assert.Equal(t, stdTx, exec)

	require.NoError(t, tx.Commit(txCtx))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestExecutor_NoTxReturnsFallback(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	assert.Same(t, db, Executor(context.Background(), db))
}

func TestExecutor_PanicsWhenPGXTransactionIsActive(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// A pgx transactor opened the transaction, but this repository talks
	// database/sql: falling back to the pool would escape the transaction.
	ctx := InjectPGXTx(context.Background(), &mockPgxTx{})
	assert.PanicsWithValue(t,
		"db: context carries a pgx transaction (*db.mockPgxTx) but the repository uses database/sql; use PGXExecutor or begin transactions with SQLTransactor",
		func() { Executor(ctx, db) })
}

func TestSQLTransactor_PassesTxOptions(t *testing.T) {
	opts := &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true}
	var got *sql.TxOptions
	beginner := &mockSQLDB{beginTx: func(ctx context.Context, o *sql.TxOptions) (*sql.Tx, error) {
		got = o
		return nil, errors.New("stop here")
	}}

	_, _, err := NewSQLTransactor(beginner, WithSQLTxOptions(opts)).BeginTx(context.Background())
	require.Error(t, err)
	assert.Same(t, opts, got)
}
