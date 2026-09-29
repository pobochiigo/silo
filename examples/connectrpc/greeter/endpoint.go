package greeter

import (
	"context"

	"github.com/pobochiigo/silo/endpoint"
)

// Endpoints holds one typed endpoint per Service method. The fields are
// exported so a caller decorates them before handing them to the transport:
// typed middlewares such as telemetry.Metrics, or go-kit ones through
// telemetry.Adapt.
type Endpoints struct {
	Greet endpoint.Endpoint[*GreetRequest, *GreetResponse]
}

// MakeEndpoints builds the endpoints on svc.
func MakeEndpoints(svc Service) Endpoints {
	return Endpoints{
		Greet: makeGreetEndpoint(svc),
	}
}

func makeGreetEndpoint(svc Service) endpoint.Endpoint[*GreetRequest, *GreetResponse] {
	return func(ctx context.Context, req *GreetRequest) (*GreetResponse, error) {
		return svc.Greet(ctx, req)
	}
}
