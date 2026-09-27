// Package ledger is the domain of the uow example: accounts with a balance,
// the entries that move money between them, and the repository and service
// contracts that middlegen decorates. The repository has one implementation
// per driver family (sqlrepo, sqlxrepo, pgxrepo); the service and the
// generated middlewares are shared by all three.
package ledger

import (
	"context"
	_ "embed"
	"errors"
	"time"
)

// Schema creates the tables the example uses. It is idempotent.
//
//go:embed schema.sql
var Schema string

var (
	ErrNotFound          = errors.New("ledger: account not found")
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	ErrInvalidAmount     = errors.New("ledger: amount must be positive")
	ErrSameAccount       = errors.New("ledger: cannot transfer to the same account")
)

// Account is a ledger account; Balance is in cents.
type Account struct {
	ID      string `db:"id"`
	Owner   string `db:"owner"`
	Balance int64  `db:"balance"`
}

// Entry is one movement on an account: positive credits, negative debits.
type Entry struct {
	ID        int64     `db:"id"`
	AccountID string    `db:"account_id"`
	Amount    int64     `db:"amount"`
	Memo      string    `db:"memo"`
	CreatedAt time.Time `db:"created_at"`
}

// Repository is the persistence contract. Every method resolves its executor
// from the context (db.Executor, db.XExecutor or db.PGXExecutor), so the same
// implementation runs against the pool and inside the transaction a unit of
// work opened.
//
//go:generate go tool middlegen -type=Repository -kinds=uow_repo,logging,tracing,metrics -service=ledger
type Repository interface {
	// Reads always run immediately: on the pool inside RunWith, inside the
	// transaction within RunInTx.
	//middlegen:non-transactional
	GetAccount(ctx context.Context, id string) (*Account, error)
	//middlegen:non-transactional
	ListEntries(ctx context.Context, accountID string, limit int) ([]Entry, error)

	// Writes are queued while a RunWith action runs (the caller gets acc back)
	// and executed immediately inside a RunInTx action.
	//middlegen:metric counter:ledger_accounts_created_total
	CreateAccount(ctx context.Context, acc *Account) (*Account, error)

	// AddEntry fills entry.ID and entry.CreatedAt from the database. When the
	// call was deferred, the caller's entry is filled once the unit of work
	// commits, because the same pointer is handed to the implementation.
	AddEntry(ctx context.Context, entry *Entry) (*Entry, error)

	// AdjustBalance adds delta to the balance; the database rejects negatives.
	//middlegen:metric attr:direction=direction(delta)
	AdjustBalance(ctx context.Context, id string, delta int64) error
}

// direction labels balance adjustments for the metrics middleware. Two
// values keep the metric's cardinality bounded.
func direction(delta int64) string {
	if delta < 0 {
		return "debit"
	}
	return "credit"
}

// Service is the business contract. The uow_service middleware wraps every
// method in a unit of work: OpenAccount and Statement in RunWith, Transfer in
// RunInTx because it must read and write under one isolation level.
//
//go:generate go tool middlegen -type=Service -kinds=uow_service,logging,tracing -service=ledger
type Service interface {
	// OpenAccount creates an account and records its opening balance. Both
	// writes are queued and committed together when the method returns.
	OpenAccount(ctx context.Context, id, owner string, opening int64) (*Account, error)

	// Transfer moves amount from one account to another. The balance check
	// and the four writes share one SERIALIZABLE transaction; a serialization
	// failure re-runs the whole method.
	//middlegen:in-tx
	Transfer(ctx context.Context, from, to string, amount int64) error

	// Statement reads an account and its latest entries. Nothing is deferred,
	// so no transaction is opened.
	Statement(ctx context.Context, id string) (*Account, []Entry, error)
}
