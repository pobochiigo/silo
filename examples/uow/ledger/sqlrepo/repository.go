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

func (r *Repository) AdjustBalance(ctx context.Context, id string, delta int64) error {
	res, err := db.Executor(ctx, r.pool).ExecContext(ctx,
		`UPDATE accounts SET balance = balance + $1 WHERE id = $2`, delta, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ledger.ErrNotFound
	}
	return nil
}
