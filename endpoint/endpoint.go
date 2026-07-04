package endpoint

import "context"

// Endpoint is a type-safe generic endpoint signature.
type Endpoint[Req any, Resp any] func(ctx context.Context, request Req) (Resp, error)
