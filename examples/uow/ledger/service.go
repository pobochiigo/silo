package ledger

import (
	"context"
	"errors"
	"fmt"
)

type service struct {
	repo Repository
}

// NewService returns the Service implementation on top of repo. Wire it with
// ServiceUoWMiddleware so its methods run inside unit of work boundaries.
func NewService(repo Repository) Service {
	return &service{repo: repo}
}

func (s *service) OpenAccount(ctx context.Context, id, owner string, opening int64) (*Account, error) {
	if opening < 0 {
		return nil, ErrInvalidAmount
	}
	// Inside RunWith both calls are queued; acc comes straight back.
	acc, err := s.repo.CreateAccount(ctx, &Account{ID: id, Owner: owner, Balance: opening})
	if err != nil {
		return nil, err
	}
	if opening > 0 {
		if _, err := s.repo.AddEntry(ctx, &Entry{AccountID: id, Amount: opening, Memo: "opening balance"}); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func (s *service) Transfer(ctx context.Context, from, to string, amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	if from == to {
		return ErrSameAccount
	}

	// Both reads run on the pool, before any transaction: they give precise
	// not-found errors. The balance is deliberately not compared here, since
	// it could change before the debit runs; the debit checks it atomically.
	if _, err := s.repo.GetAccount(ctx, from); err != nil {
		return fmt.Errorf("source %s: %w", from, err)
	}
	if _, err := s.repo.GetAccount(ctx, to); err != nil {
		return fmt.Errorf("destination %s: %w", to, err)
	}

	// Inside RunWith the four writes are queued; they run in one transaction
	// after this method returns. If the debit finds the balance short it
	// fails with ErrInsufficientFunds and the whole unit of work rolls back.
	if err := s.repo.AdjustBalance(ctx, from, -amount); err != nil {
		return err
	}
	if err := s.repo.AdjustBalance(ctx, to, amount); err != nil {
		return err
	}
	if _, err := s.repo.AddEntry(ctx, &Entry{AccountID: from, Amount: -amount, Memo: "transfer to " + to}); err != nil {
		return err
	}
	_, err := s.repo.AddEntry(ctx, &Entry{AccountID: to, Amount: amount, Memo: "transfer from " + from})
	return err
}

func (s *service) ApplyInterest(ctx context.Context, rateBps int64) error {
	if rateBps <= 0 {
		return ErrInvalidAmount
	}

	// This method is marked //middlegen:in-tx, so it runs as one task: the
	// transaction is already open, ListAccounts reads under its isolation
	// level, and every write below executes immediately with a real result.
	accounts, err := s.repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		interest := acc.Balance * rateBps / 10_000
		if interest == 0 {
			continue
		}
		if err := s.repo.AdjustBalance(ctx, acc.ID, interest); err != nil {
			return err
		}
		entry, err := s.repo.AddEntry(ctx, &Entry{AccountID: acc.ID, Amount: interest, Memo: fmt.Sprintf("interest %d bps", rateBps)})
		if err != nil {
			return err
		}
		if entry.ID == 0 {
			return errors.New("ledger: AddEntry returned no id although it ran inside the transaction")
		}
	}
	return nil
}

func (s *service) Statement(ctx context.Context, id string) (*Account, []Entry, error) {
	acc, err := s.repo.GetAccount(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	entries, err := s.repo.ListEntries(ctx, id, 5)
	if err != nil {
		return nil, nil, err
	}
	return acc, entries, nil
}
