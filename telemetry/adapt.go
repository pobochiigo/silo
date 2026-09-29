package telemetry

import (
	"context"
	"fmt"

	kitendpoint "github.com/go-kit/kit/endpoint"

	"github.com/pobochiigo/silo/endpoint"
	"github.com/pobochiigo/silo/middleware"
)

// Adapt turns a go-kit endpoint middleware into a middleware for a typed
// endpoint, so TracingMiddleware, LoggingMiddleware and MetricsMiddleware, or
// any other go-kit middleware, decorate an endpoint.Endpoint[Req, Resp] the
// way they decorate a go-kit endpoint:
//
//	greet = telemetry.Adapt[GreetRequest, GreetResponse](telemetry.TracingMiddleware("greet"))(greet)
//
// It lives here rather than in the endpoint package so that package keeps no
// dependency beyond the standard library. The go-kit middleware sees the
// request and the response as any. One that replaces either with a value of
// another type makes the adapted endpoint return an error instead of
// panicking on the type assertion.
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
