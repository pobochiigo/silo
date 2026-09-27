// Package sqlrepo implements ledger.Repository with database/sql. It pairs
// with db.NewSQLTransactor and resolves its executor through db.Executor.
package sqlrepo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pobochiigo/silo/db"

	"github.com/pobochiigo/silo/examples/uow/ledger"
)

// Repository talks to PostgreSQL through database/sql.
type Repository struct {
	pool *sql.DB
}

// New returns a repository on pool. Statements run on pool unless the context
// carries a transaction, in which case they run inside it.
func New(pool *sql.DB) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) GetAccount(ctx context.Context, id string) (*ledger.Account, error) {
	var acc ledger.Account
	err := db.Executor(ctx, r.pool).QueryRowContext(ctx,
		`SELECT id, owner, balance FROM accounts WHERE id = $1`, id,
	).Scan(&acc.ID, &acc.Owner, &acc.Balance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ledger.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

func (r *Repository) ListAccounts(ctx context.Context) ([]ledger.Account, error) {
	rows, err := db.Executor(ctx, r.pool).QueryContext(ctx,
		`SELECT id, owner, balance FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []ledger.Account
	for rows.Next() {
		var acc ledger.Account
		if err := rows.Scan(&acc.ID, &acc.Owner, &acc.Balance); err != nil {
			return nil, err
		}
		accounts = append(accounts, acc)
	}
	return accounts, rows.Err()
}

func (r *Repository) ListEntries(ctx context.Context, accountID string, limit int) ([]ledger.Entry, error) {
	rows, err := db.Executor(ctx, r.pool).QueryContext(ctx,
		`SELECT id, account_id, amount, memo, created_at FROM entries WHERE account_id = $1 ORDER BY id DESC LIMIT $2`,
		accountID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ledger.Entry
	for rows.Next() {
		var e ledger.Entry
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Amount, &e.Memo, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (r *Repository) CreateAccount(ctx context.Context, acc *ledger.Account) (*ledger.Account, error) {
	_, err := db.Executor(ctx, r.pool).ExecContext(ctx,
		`INSERT INTO accounts (id, owner, balance) VALUES ($1, $2, $3)`, acc.ID, acc.Owner, acc.Balance)
	if err != nil {
		return nil, err
	}
	return acc, nil
}

func (r *Repository) AddEntry(ctx context.Context, entry *ledger.Entry) (*ledger.Entry, error) {
	err := db.Executor(ctx, r.pool).QueryRowContext(ctx,
		`INSERT INTO entries (account_id, amount, memo) VALUES ($1, $2, $3) RETURNING id, created_at`,
		entry.AccountID, entry.Amount, entry.Memo,
	).Scan(&entry.ID, &entry.CreatedAt)
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// AdjustBalance puts the check in the write: the WHERE clause refuses a debit
// the balance does not cover, so the row is updated or the statement is a
// no-op, atomically, under any isolation level.
func (r *Repository) AdjustBalance(ctx context.Context, id string, delta int64) error {
	res, err := db.Executor(ctx, r.pool).ExecContext(ctx,
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
