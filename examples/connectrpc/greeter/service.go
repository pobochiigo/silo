package greeter

import (
	"context"
	"strings"

	"connectrpc.com/connect"
)

// Service is the business contract. The server wraps an implementation in
// endpoints and a Connect handler (NewGreeterHandler); the client package
// returns one backed by Connect calls (client/greeter.NewGreeterClient). The
// two are interchangeable, and the generated decorators fit both.
//
//go:generate go tool middlegen -type=Service -kinds=logging,tracing -service=greeter
type Service interface {
	Greet(ctx context.Context, req *GreetRequest) (*GreetResponse, error)
}

type service struct{}

// NewService returns the in-process implementation. Its errors pass through
// the adapters untouched, so it decides its own Connect code when it wants
// one.
func NewService() Service {
	return service{}
}

func (service) Greet(_ context.Context, req *GreetRequest) (*GreetResponse, error) {
	if strings.EqualFold(req.Name, "nobody") {
		return nil, connect.NewError(connect.CodeNotFound, ErrUnknownPerson)
	}
	return &GreetResponse{Greeting: "Hello, " + req.Name}, nil
}
