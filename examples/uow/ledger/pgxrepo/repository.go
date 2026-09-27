// Package pgxrepo implements ledger.Repository with native pgx. It pairs
// with db.NewPGXTransactor and resolves its executor through db.PGXExecutor,
// which yields the pool or the open pgx.Tx.
package pgxrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/pobochiigo/silo/db"

	"github.com/pobochiigo/silo/examples/uow/ledger"
)

// Repository talks to PostgreSQL through pgx.
type Repository struct {
	pool db.PGXCommon
}

// New returns a repository on pool, typically a *pgxpool.Pool.
func New(pool db.PGXCommon) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) GetAccount(ctx context.Context, id string) (*ledger.Account, error) {
	var acc ledger.Account
	err := db.PGXExecutor(ctx, r.pool).QueryRow(ctx,
		`SELECT id, owner, balance FROM accounts WHERE id = $1`, id,
	).Scan(&acc.ID, &acc.Owner, &acc.Balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ledger.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

func (r *Repository) ListAccounts(ctx context.Context) ([]ledger.Account, error) {
	rows, err := db.PGXExecutor(ctx, r.pool).Query(ctx,
		`SELECT id, owner, balance FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ledger.Account])
}

func (r *Repository) ListEntries(ctx context.Context, accountID string, limit int) ([]ledger.Entry, error) {
	rows, err := db.PGXExecutor(ctx, r.pool).Query(ctx,
		`SELECT id, account_id, amount, memo, created_at FROM entries WHERE account_id = $1 ORDER BY id DESC LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ledger.Entry])
}

func (r *Repository) CreateAccount(ctx context.Context, acc *ledger.Account) (*ledger.Account, error) {
	_, err := db.PGXExecutor(ctx, r.pool).Exec(ctx,
		`INSERT INTO accounts (id, owner, balance) VALUES ($1, $2, $3)`, acc.ID, acc.Owner, acc.Balance)
	if err != nil {
		return nil, err
	}
	return acc, nil
}

func (r *Repository) AddEntry(ctx context.Context, entry *ledger.Entry) (*ledger.Entry, error) {
	err := db.PGXExecutor(ctx, r.pool).QueryRow(ctx,
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
	tag, err := db.PGXExecutor(ctx, r.pool).Exec(ctx,
		`UPDATE accounts SET balance = balance + $1 WHERE id = $2 AND balance + $1 >= 0`, delta, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
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
