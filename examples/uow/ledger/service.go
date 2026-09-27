package ledger

import (
	"context"
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

	// RunInTx: these reads run inside the transaction, and the writes below
	// are queued and run in the same transaction before it commits, so the
	// check and the writes are covered by one SERIALIZABLE snapshot.
	src, err := s.repo.GetAccount(ctx, from)
	if err != nil {
		return fmt.Errorf("source %s: %w", from, err)
	}
	if _, err := s.repo.GetAccount(ctx, to); err != nil {
		return fmt.Errorf("destination %s: %w", to, err)
	}
	if src.Balance < amount {
		return fmt.Errorf("%w: %s holds %d, transfer needs %d", ErrInsufficientFunds, from, src.Balance, amount)
	}

	if err := s.repo.AdjustBalance(ctx, from, -amount); err != nil {
		return err
	}
	if err := s.repo.AdjustBalance(ctx, to, amount); err != nil {
		return err
	}
	if _, err := s.repo.AddEntry(ctx, &Entry{AccountID: from, Amount: -amount, Memo: "transfer to " + to}); err != nil {
		return err
	}
	_, err = s.repo.AddEntry(ctx, &Entry{AccountID: to, Amount: amount, Memo: "transfer from " + from})
	return err
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
