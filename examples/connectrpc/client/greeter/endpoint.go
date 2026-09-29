// Package greeter is the Connect client of the greeter service. It returns
// the Service interface the server implements, backed by typed endpoints
// over the generated Connect client, so the decorators written for the
// service fit the client as well.
package greeter

import (
	"context"

	"github.com/pobochiigo/silo/endpoint"

	bizgreeter "github.com/pobochiigo/silo/examples/connectrpc/greeter"
)

// endpoints implements bizgreeter.Service by calling one endpoint per method.
type endpoints struct {
	greet endpoint.Endpoint[*bizgreeter.GreetRequest, *bizgreeter.GreetResponse]
}

func (c *endpoints) Greet(ctx context.Context, req *bizgreeter.GreetRequest) (*bizgreeter.GreetResponse, error) {
	return c.greet(ctx, req)
}
