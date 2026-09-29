package telemetry

import (
	"context"
	"fmt"

	kitendpoint "github.com/go-kit/kit/endpoint"

	"github.com/pobochiigo/silo/endpoint"
	"github.com/pobochiigo/silo/middleware"
)

// Kit converts a middleware over endpoint.Endpoint[any, any] into a go-kit
// endpoint middleware. go-kit's endpoint type is Endpoint[any, any] under
// another name, so this is a type conversion: no assertion, no extra call.
// It is how Tracing, Logging and Metrics, or any typed middleware
// instantiated with [any, any], decorate a go-kit endpoint:
//
//	ep = telemetry.Kit(telemetry.Tracing[any, any]("greet"))(ep)
func Kit(mw middleware.Middleware[endpoint.Endpoint[any, any]]) kitendpoint.Middleware {
	return func(next kitendpoint.Endpoint) kitendpoint.Endpoint {
		return kitendpoint.Endpoint(mw(endpoint.Endpoint[any, any](next)))
	}
}

// Adapt is the other direction: it turns a go-kit endpoint middleware, such
// as go-kit's rate limiter or circuit breaker, into a middleware for a typed
// endpoint:
//
//	greet = telemetry.Adapt[GreetRequest, GreetResponse](ratelimit.NewErroringLimiter(limiter))(greet)
//
// The go-kit middleware sees the request and the response as any, so this
// direction needs a type assertion on the way back. One that replaces the
// request or the response with a value of another type makes the adapted
// endpoint return an error instead of panicking. Tracing, Logging and
// Metrics need no adapting: instantiate them with the endpoint's types.
//
// It lives here rather than in the endpoint package so that package keeps no
// dependency beyond the standard library.
func Adapt[Req any, Resp any](mw kitendpoint.Middleware) middleware.Middleware[endpoint.Endpoint[Req, Resp]] {
	return func(next endpoint.Endpoint[Req, Resp]) endpoint.Endpoint[Req, Resp] {
		untyped := mw(func(ctx context.Context, request any) (any, error) {
			req, ok := request.(Req)
			if !ok && request != nil {
				return nil, fmt.Errorf("telemetry: middleware passed a %T request, want %T", request, req)
			}
			return next(ctx, req)
		})
		return func(ctx context.Context, request Req) (Resp, error) {
			response, err := untyped(ctx, request)
			resp, ok := response.(Resp)
			if !ok && response != nil {
				return resp, fmt.Errorf("telemetry: middleware returned a %T response, want %T", response, resp)
			}
			return resp, err
		}
	}
}
