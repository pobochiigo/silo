package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	kitendpoint "github.com/go-kit/kit/endpoint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type greetReq struct{ Name string }

type greetResp struct{ Greeting string }

func greet(_ context.Context, r greetReq) (greetResp, error) {
	if r.Name == "" {
		return greetResp{}, errors.New("empty name")
	}
	return greetResp{Greeting: "hello " + r.Name}, nil
}

func passthrough(next kitendpoint.Endpoint) kitendpoint.Endpoint { return next }

func TestAdapt(t *testing.T) {
	t.Run("the go-kit middleware sees the typed values and the endpoint keeps its types", func(t *testing.T) {
		var seenReq, seenResp any
		mw := func(next kitendpoint.Endpoint) kitendpoint.Endpoint {
			return func(ctx context.Context, request any) (any, error) {
				seenReq = request
				response, err := next(ctx, request)
				seenResp = response
				return response, err
			}
		}

		ep := Adapt[greetReq, greetResp](mw)(greet)
		got, err := ep(context.Background(), greetReq{Name: "ada"})
		require.NoError(t, err)
		assert.Equal(t, greetResp{Greeting: "hello ada"}, got)
		assert.Equal(t, greetReq{Name: "ada"}, seenReq)
		assert.Equal(t, greetResp{Greeting: "hello ada"}, seenResp)
	})

	t.Run("endpoint errors pass through with the zero response", func(t *testing.T) {
		got, err := Adapt[greetReq, greetResp](passthrough)(greet)(context.Background(), greetReq{})
		assert.EqualError(t, err, "empty name")
		assert.Equal(t, greetResp{}, got)
	})

	t.Run("a middleware that short-circuits with a nil response yields the zero value", func(t *testing.T) {
		boom := errors.New("rate limited")
		mw := func(kitendpoint.Endpoint) kitendpoint.Endpoint {
			return func(context.Context, any) (any, error) { return nil, boom }
		}
		got, err := Adapt[greetReq, greetResp](mw)(greet)(context.Background(), greetReq{Name: "ada"})
		assert.ErrorIs(t, err, boom)
		assert.Equal(t, greetResp{}, got)
	})

	t.Run("a middleware that swaps the response type reports it instead of panicking", func(t *testing.T) {
		mw := func(kitendpoint.Endpoint) kitendpoint.Endpoint {
			return func(context.Context, any) (any, error) { return "cached", nil }
		}
		got, err := Adapt[greetReq, greetResp](mw)(greet)(context.Background(), greetReq{Name: "ada"})
		assert.ErrorContains(t, err, "returned a string response, want telemetry.greetResp")
		assert.Equal(t, greetResp{}, got)
	})

	t.Run("a middleware that swaps the request type reports it instead of panicking", func(t *testing.T) {
		mw := func(next kitendpoint.Endpoint) kitendpoint.Endpoint {
			return func(ctx context.Context, _ any) (any, error) { return next(ctx, 42) }
		}
		_, err := Adapt[greetReq, greetResp](mw)(greet)(context.Background(), greetReq{Name: "ada"})
		assert.ErrorContains(t, err, "passed a int request, want telemetry.greetReq")
	})

	t.Run("nil interface values are handed through", func(t *testing.T) {
		var calledWith error
		ep := Adapt[error, error](passthrough)(
			func(_ context.Context, in error) (error, error) { calledWith = in; return nil, nil })
		got, err := ep(context.Background(), nil)
		require.NoError(t, err)
		assert.Nil(t, got)
		assert.Nil(t, calledWith)
	})

	t.Run("composes with the package's own middlewares", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		ep := Adapt[greetReq, greetResp](LoggingMiddleware("greet", logger))(greet)
		got, err := ep(context.Background(), greetReq{Name: "ada"})
		require.NoError(t, err)
		assert.Equal(t, greetResp{Greeting: "hello ada"}, got)
		assert.Contains(t, buf.String(), "endpoint execution succeeded")
		assert.Contains(t, buf.String(), "operation=greet")
	})
}
