package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/pobochiigo/silo/endpoint"
	"github.com/pobochiigo/silo/middleware"

	greeterv1 "github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1"
)

// The business types know nothing about protobuf or Connect.
type GreetRequest struct {
	Name string
}

type GreetResponse struct {
	Greeting string
}

var ErrUnknownPerson = errors.New("greeter: unknown person")

// greet is the business endpoint. Its errors pass through the adapter
// untouched, so it decides its own Connect code when it wants one.
func greet(_ context.Context, req GreetRequest) (GreetResponse, error) {
	if strings.EqualFold(req.Name, "nobody") {
		return GreetResponse{}, connect.NewError(connect.CodeNotFound, ErrUnknownPerson)
	}
	return GreetResponse{Greeting: "Hello, " + req.Name}, nil
}

// decodeGreet converts the wire message into the business request. A failure
// here is reported to the client as CodeInvalidArgument.
func decodeGreet(_ context.Context, msg *greeterv1.GreetRequest) (GreetRequest, error) {
	if msg.GetName() == "" {
		return GreetRequest{}, errors.New("name is required")
	}
	return GreetRequest{Name: msg.GetName()}, nil
}

func encodeGreet(_ context.Context, resp GreetResponse) (*greeterv1.GreetResponse, error) {
	return &greeterv1.GreetResponse{Greeting: resp.Greeting}, nil
}

// Client-side codecs: business request to wire message and back.
func encodeGreetRequest(_ context.Context, req GreetRequest) (*greeterv1.GreetRequest, error) {
	return &greeterv1.GreetRequest{Name: req.Name}, nil
}

func decodeGreetResponse(_ context.Context, msg *greeterv1.GreetResponse) (GreetResponse, error) {
	return GreetResponse{Greeting: msg.GetGreeting()}, nil
}

// timing is a hand-written middleware for typed endpoints, built on the same
// generic Middleware type the generated decorators use.
func timing[Req, Resp any](label string) middleware.Middleware[endpoint.Endpoint[Req, Resp]] {
	return func(next endpoint.Endpoint[Req, Resp]) endpoint.Endpoint[Req, Resp] {
		return func(ctx context.Context, req Req) (Resp, error) {
			start := time.Now()
			resp, err := next(ctx, req)
			fmt.Printf("   [%s] %s err=%v\n", label, time.Since(start).Round(time.Microsecond), err)
			return resp, err
		}
	}
}
