// Package endpoint defines a type-safe endpoint signature. It depends on the
// standard library only, so SDKs built on it stay lean; go-kit endpoint
// middlewares are applied to it through telemetry.Adapt.
package endpoint

import "context"

// Endpoint is a type-safe generic endpoint signature.
type Endpoint[Req any, Resp any] func(ctx context.Context, request Req) (Resp, error)
