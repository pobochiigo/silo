package inventory

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Memory is an in-memory Repository. It prints every write it executes so
// the example can show when deferred calls really run.
type Memory struct {
	mu       sync.Mutex
	items    map[string]*Item
	archived map[string]Item
	keys     map[string]string
	out      io.Writer
}

// NewMemory returns an empty repository that reports executed writes on out.
func NewMemory(out io.Writer) *Memory {
	return &Memory{items: map[string]*Item{}, archived: map[string]Item{}, keys: map[string]string{}, out: out}
}

func (m *Memory) executed(format string, args ...any) {
	fmt.Fprintf(m.out, "   [repo] "+format+"\n", args...)
}

func (m *Memory) Close() error {
	m.executed("Close executed")
	return nil
}

func (m *Memory) Get(_ context.Context, sku string) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[sku]
	if !ok {
		return nil, ErrNotFound
	}
	copied := *item
	return &copied, nil
}

func (m *Memory) List(context.Context) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]Item, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, *item)
	}
	return items, nil
}

func (m *Memory) Save(_ context.Context, item *Item) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *item
	m.items[item.SKU] = &copied
	m.executed("Save executed: %s quantity=%d reserved=%d", item.SKU, item.Quantity, item.Reserved)
	return &copied, nil
}

func (m *Memory) Merge(_ context.Context, from, into *Item) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	merged := *into
	merged.Quantity += from.Quantity
	m.items[into.SKU] = &merged
	delete(m.items, from.SKU)
	m.executed("Merge executed: %s into %s, quantity now %d", from.SKU, into.SKU, merged.Quantity)
	return &merged, nil
}

func (m *Memory) Archive(_ context.Context, item *Item) (*Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archived[item.SKU] = *item
	delete(m.items, item.SKU)
	m.executed("Archive executed: %s", item.SKU)
	return item, nil
}

func (m *Memory) RotateKey(_ context.Context, sku string, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[sku]; !ok {
		return ErrNotFound
	}
	m.keys[sku] = key
	m.executed("RotateKey executed: %s (%d-character key)", sku, len(key))
	return nil
}

func (m *Memory) Decrement(_ context.Context, sku string, qty int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[sku]
	if !ok {
		return 0, ErrNotFound
	}
	if item.Quantity < qty {
		return item.Quantity, ErrInsufficientStock
	}
	item.Quantity -= qty
	m.executed("Decrement executed: %s by %d, quantity now %d", sku, qty, item.Quantity)
	return item.Quantity, nil
}
