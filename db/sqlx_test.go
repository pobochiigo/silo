package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type userRow struct {
	ID   string `db:"id"`
	Name string `db:"name"`
}

func newSQLX(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return sqlx.NewDb(db, "sqlmock"), mock
}

func userRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name"}).AddRow("u1", "Alice")
}

func TestSQLXTransactor_InjectsGenuineSQLXTx(t *testing.T) {
	xdb, mock := newSQLX(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, name FROM users").WillReturnRows(userRows())
	mock.ExpectCommit()

	tx, txCtx, err := NewSQLXTransactor(xdb).BeginTx(context.Background())
	require.NoError(t, err)

	exec := XExecutor(txCtx, xdb)
	_, isSQLXTx := exec.(*sqlx.Tx)
	assert.True(t, isSQLXTx, "expected the genuine *sqlx.Tx, not a wrapper")
	fromCtx, ok := ExtractTx(txCtx)
	require.True(t, ok)
	assert.Same(t, fromCtx, exec)

	var r userRow
	require.NoError(t, exec.GetContext(txCtx, &r, "SELECT id, name FROM users WHERE id = ?", "u1"))
	assert.Equal(t, userRow{ID: "u1", Name: "Alice"}, r)

	require.NoError(t, tx.Commit(txCtx))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestXExecutor_WrapsStdTxWithPoolMapper(t *testing.T) {
	xdb, mock := newSQLX(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, name FROM users").WillReturnRows(userRows())
	mock.ExpectRollback()

	// Plain database/sql transactor: the context carries a *sql.Tx.
	tx, txCtx, err := NewSQLTransactor(xdb.DB).BeginTx(context.Background())
	require.NoError(t, err)

	exec := XExecutor(txCtx, xdb)
	wrapped, ok := exec.(*sqlx.Tx)
	require.True(t, ok)
	assert.Same(t, xdb.Mapper, wrapped.Mapper, "wrapper must inherit the pool's mapper")

	var r userRow
	require.NoError(t, exec.GetContext(txCtx, &r, "SELECT id, name FROM users"))
	assert.Equal(t, userRow{ID: "u1", Name: "Alice"}, r)

	require.NoError(t, tx.Rollback(txCtx))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// wrappedPool satisfies SQLXCommon without being a *sqlx.DB.
type wrappedPool struct{ *sqlx.DB }

func TestXExecutor_WrapsStdTxWithDefaultMapperForNonSQLXFallback(t *testing.T) {
	xdb, mock := newSQLX(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, name FROM users").WillReturnRows(userRows())
	mock.ExpectRollback()

	tx, txCtx, err := NewSQLTransactor(xdb.DB).BeginTx(context.Background())
	require.NoError(t, err)

	exec := XExecutor(txCtx, wrappedPool{xdb})
	wrapped, ok := exec.(*sqlx.Tx)
	require.True(t, ok)
	require.NotNil(t, wrapped.Mapper, "a nil mapper panics on struct scans")

	// Regression: this used to dereference a nil *reflectx.Mapper.
	var r userRow
	require.NoError(t, exec.GetContext(txCtx, &r, "SELECT id, name FROM users"))
	assert.Equal(t, userRow{ID: "u1", Name: "Alice"}, r)

	require.NoError(t, tx.Rollback(txCtx))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestXExecutor_NoTxReturnsFallback(t *testing.T) {
	xdb, _ := newSQLX(t)
	assert.Same(t, xdb, XExecutor(context.Background(), xdb))
}

// foreignExecutor is an SQLCommon that XExecutor cannot adapt to sqlx.
type foreignExecutor struct{ SQLCommon }

func TestXExecutor_PanicsOnUnknownExecutor(t *testing.T) {
	xdb, _ := newSQLX(t)
	ctx := InjectTx(context.Background(), foreignExecutor{})

	// Silently returning the pool would run statements outside the transaction.
	assert.Panics(t, func() { XExecutor(ctx, xdb) })
}

func TestXExecutor_PanicsWhenPGXTransactionIsActive(t *testing.T) {
	xdb, _ := newSQLX(t)
	ctx := InjectPGXTx(context.Background(), &mockPgxTx{})

	assert.Panics(t, func() { XExecutor(ctx, xdb) })
}

func TestSQLXTransactor_PassesTxOptions(t *testing.T) {
	xdb, mock := newSQLX(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	transactor := NewSQLXTransactor(xdb, WithSQLXTxOptions(opts))
	assert.Same(t, opts, transactor.opts)

	tx, txCtx, err := transactor.BeginTx(context.Background())
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(txCtx))
	assert.NoError(t, mock.ExpectationsWereMet())
}
