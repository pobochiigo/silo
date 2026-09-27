// Package sqlxrepo implements ledger.Repository with sqlx: struct scanning
// and named queries. It pairs with db.NewSQLXTransactor, which injects a
// genuine *sqlx.Tx so named queries keep the driver's $N bindvars inside
// transactions, and resolves its executor through db.XExecutor.
package sqlxrepo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"

	"github.com/pobochiigo/silo/db"

	"github.com/pobochiigo/silo/examples/uow/ledger"
)

// Repository talks to PostgreSQL through sqlx.
type Repository struct {
	pool *sqlx.DB
}

// New returns a repository on pool.
func New(pool *sqlx.DB) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) GetAccount(ctx context.Context, id string) (*ledger.Account, error) {
	var acc ledger.Account
	err := db.XExecutor(ctx, r.pool).GetContext(ctx, &acc,
		`SELECT id, owner, balance FROM accounts WHERE id = $1`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ledger.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

func (r *Repository) ListAccounts(ctx context.Context) ([]ledger.Account, error) {
	var accounts []ledger.Account
	err := db.XExecutor(ctx, r.pool).SelectContext(ctx, &accounts,
		`SELECT id, owner, balance FROM accounts ORDER BY id`)
	return accounts, err
}

func (r *Repository) ListEntries(ctx context.Context, accountID string, limit int) ([]ledger.Entry, error) {
	var entries []ledger.Entry
	err := db.XExecutor(ctx, r.pool).SelectContext(ctx, &entries,
		`SELECT id, account_id, amount, memo, created_at FROM entries WHERE account_id = $1 ORDER BY id DESC LIMIT $2`,
		accountID, limit)
	return entries, err
}

func (r *Repository) CreateAccount(ctx context.Context, acc *ledger.Account) (*ledger.Account, error) {
	// A named query binds the struct's db tags; it needs the driver name the
	// genuine *sqlx.Tx carries, which is why this pairs with SQLXTransactor.
	_, err := db.XExecutor(ctx, r.pool).NamedExecContext(ctx,
		`INSERT INTO accounts (id, owner, balance) VALUES (:id, :owner, :balance)`, acc)
	if err != nil {
		return nil, err
	}
	return acc, nil
}

func (r *Repository) AddEntry(ctx context.Context, entry *ledger.Entry) (*ledger.Entry, error) {
	// sqlx has no NamedQueryContext method on transactions, but the executor
	// satisfies sqlx.ExtContext, so the package-level helper works on both.
	rows, err := sqlx.NamedQueryContext(ctx, db.XExecutor(ctx, r.pool),
		`INSERT INTO entries (account_id, amount, memo) VALUES (:account_id, :amount, :memo) RETURNING id, created_at`, entry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	if err := rows.StructScan(entry); err != nil {
		return nil, err
	}
	return entry, rows.Err()
}

// AdjustBalance puts the check in the write: the WHERE clause refuses a debit
// the balance does not cover, so the row is updated or the statement is a
// no-op, atomically, under any isolation level.
func (r *Repository) AdjustBalance(ctx context.Context, id string, delta int64) error {
	res, err := db.XExecutor(ctx, r.pool).ExecContext(ctx,
		`UPDATE accounts SET balance = balance + $1 WHERE id = $2 AND balance + $1 >= 0`, delta, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return shortOrMissing(delta)
	}
	return nil
}

// shortOrMissing interprets an update that touched no row: a debit means the
// balance was short (the service verified the account exists); a credit can
// only miss because the account does not exist.
func shortOrMissing(delta int64) error {
	if delta < 0 {
		return ledger.ErrInsufficientFunds
	}
	return ledger.ErrNotFound
}
