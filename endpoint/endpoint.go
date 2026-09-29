// Package endpoint defines a type-safe endpoint signature. It depends on the
// standard library only, so SDKs built on it stay lean. The telemetry
// package's Tracing, Logging and Metrics decorate it directly; go-kit's
// endpoint type is Endpoint[any, any] under another name, which
// telemetry.Kit and telemetry.Adapt convert in either direction.
package endpoint

import "context"

// Endpoint is a type-safe generic endpoint signature.
type Endpoint[Req any, Resp any] func(ctx context.Context, request Req) (Resp, error)
