// Package example is the fixture interface for the middlegen golden tests.
// It exercises every directive, embedded interfaces, and the signature
// shapes the templates special-case; edit it together with the golden files.
package example

import (
	"context"
	"fmt"
	"io"
	"time"
)

type Item struct{ ID string }

// Finder is embedded by Example; directives on its methods carry over.
type Finder interface {
	//middlegen:non-transactional
	Find(ctx context.Context, id string) (*Item, error)
}

type Example interface {
	io.Closer    // standard library embedded interface: Close() error is decorated
	fmt.Stringer // String() string
	Finder       // same-package embedded interface, expanded in place

	Ping() error

	Name(ctx context.Context) string

	Fire(ctx context.Context)

	//middlegen:metric attr:item_id=item.ID
	//middlegen:metric counter:saves_total
	Save(ctx context.Context, item *Item) (*Item, error)

	//middlegen:echo dst
	Copy(ctx context.Context, src *Item, dst *Item) (*Item, error)

	//middlegen:redact secret
	Rotate(ctx context.Context, name string, secret string) (string, bool, error)

	Materialize(c context.Context, t *Item, ok bool) (Item, error)

	Schedule(ctx context.Context, at time.Time, items ...*Item) error

	// blank parameter name, context not in first position
	Tag(_ string, ctx context.Context) error

	// uow_service: runs as one task through RunInTx instead of a RunWith boundary
	//middlegen:in-tx
	Transfer(ctx context.Context, from, to string, amount int64) error
}
