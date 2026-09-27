package inventory

import (
	"context"
	"fmt"
)

type service struct {
	repo Repository
}

// NewService returns the Service implementation on top of repo.
func NewService(repo Repository) Service {
	return &service{repo: repo}
}

func (s *service) Restock(ctx context.Context, sku string, qty int) (*Item, error) {
	item, err := s.repo.Get(ctx, sku) // immediate: non-transactional
	if err != nil {
		item = &Item{SKU: sku, Name: sku}
	}
	item.Quantity += qty
	return s.repo.Save(ctx, item) // queued inside RunWith: returns item itself
}

func (s *service) Reserve(ctx context.Context, sku string, qty int) error {
	item, err := s.repo.Get(ctx, sku)
	if err != nil {
		return err
	}
	if item.Quantity-item.Reserved < qty {
		return fmt.Errorf("%w: %s has %d available, %d requested", ErrInsufficientStock, sku, item.Quantity-item.Reserved, qty)
	}
	item.Reserved += qty
	_, err = s.repo.Save(ctx, item) // queued; runs inside the RunInTx transaction before it commits
	return err
}

func (s *service) Rotate(ctx context.Context, sku string, key string) error {
	return s.repo.RotateKey(ctx, sku, key)
}
