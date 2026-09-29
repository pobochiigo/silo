package greeter

import (
	"context"

	kitendpoint "github.com/go-kit/kit/endpoint"

	"github.com/pobochiigo/silo/endpoint"
	"github.com/pobochiigo/silo/telemetry"
)

// Endpoints holds one typed endpoint per Service method.
type Endpoints struct {
	Greet endpoint.Endpoint[*GreetRequest, *GreetResponse]
}

// MakeEndpoints builds the endpoints on svc. The go-kit middlewares in mws
// wrap every endpoint through telemetry.Adapt, the first one outermost, so
// one set of middlewares serves every method whatever its types. An SDK that
// wants no go-kit dependency drops the parameter.
func MakeEndpoints(svc Service, mws ...kitendpoint.Middleware) Endpoints {
	return Endpoints{
		Greet: wrap(makeGreetEndpoint(svc), mws),
	}
}

func makeGreetEndpoint(svc Service) endpoint.Endpoint[*GreetRequest, *GreetResponse] {
	return func(ctx context.Context, req *GreetRequest) (*GreetResponse, error) {
		return svc.Greet(ctx, req)
	}
}

// wrap applies mws to ep, the first middleware outermost.
func wrap[Req, Resp any](ep endpoint.Endpoint[Req, Resp], mws []kitendpoint.Middleware) endpoint.Endpoint[Req, Resp] {
	if len(mws) == 0 {
		return ep
	}
	return telemetry.Adapt[Req, Resp](kitendpoint.Chain(mws[0], mws[1:]...))(ep)
}
