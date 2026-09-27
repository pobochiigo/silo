// Package inventory is the domain of the middlegen example. Repository uses
// every directive middlegen understands and embeds interfaces from this
// package and the standard library; Memory implements it without a database
// so the example runs anywhere.
package inventory

import (
	"context"
	"errors"
	"io"
)

var (
	ErrNotFound          = errors.New("inventory: unknown sku")
	ErrInsufficientStock = errors.New("inventory: insufficient stock")
)

// Item is a stock keeping unit and its quantities.
type Item struct {
	SKU      string
	Name     string
	Quantity int
	Reserved int
}

// Reader is embedded by Repository; the directives on its methods carry over
// to the generated code.
type Reader interface {
	//middlegen:non-transactional
	Get(ctx context.Context, sku string) (*Item, error)

	List(ctx context.Context) ([]Item, error) //middlegen:non-transactional
}

// Repository is decorated with all four kinds. Reads run immediately; writes
// are queued on the unit of work and run when its transaction commits, in
// RunWith and RunInTx alike.
//
//go:generate go tool middlegen -type=Repository -kinds=uow_repo,logging,tracing,metrics -service=inventory
type Repository interface {
	io.Closer // no context: logged and measured, never traced or deferred
	Reader

	// Exactly one parameter has the result's type, so item is echoed back
	// while the write is queued.
	//middlegen:metric counter:inventory_saves_total
	//middlegen:metric attr:stock=stockOf(item)
	Save(ctx context.Context, item *Item) (*Item, error)

	// Two parameters match the result: the directive names the one to return.
	//middlegen:echo into
	Merge(ctx context.Context, from, into *Item) (*Item, error)

	// Echoing switched off: nil comes back until the write happens.
	//middlegen:echo none
	Archive(ctx context.Context, item *Item) (*Item, error)

	// The key is logged as [REDACTED].
	//middlegen:redact key
	RotateKey(ctx context.Context, sku string, key string) error

	// Basic-typed results are never echoed: zero values while queued.
	Decrement(ctx context.Context, sku string, qty int) (int, error)
}

// stockOf labels saves for the metrics middleware with a bounded set of values.
func stockOf(item *Item) string {
	if item == nil || item.Quantity == 0 {
		return "empty"
	}
	return "stocked"
}

// Service is decorated from another package (svcmw) to show the -dir flag.
// Restock uses the deferred-write model, Reserve the transactional one.
type Service interface {
	// Restock reads the item now and queues the write; RunWith commits it
	// after the method returns.
	Restock(ctx context.Context, sku string, qty int) (*Item, error)

	// Reserve checks and updates stock under one transaction: the read and
	// the queued write share its isolation level, so a concurrent reservation
	// cannot slip in between them.
	//middlegen:in-tx
	Reserve(ctx context.Context, sku string, qty int) error

	// Rotate shows the redaction directive end to end.
	Rotate(ctx context.Context, sku string, key string) error
}
