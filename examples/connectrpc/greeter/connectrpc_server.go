package greeter

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	kitendpoint "github.com/go-kit/kit/endpoint"

	"github.com/pobochiigo/silo/connectrpc"

	greeterv1 "github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1"
	"github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1/greeterv1connect"
)

// server implements the generated handler interface by delegating each
// method to its adapted endpoint.
type server struct {
	greet connectrpc.Handler[greeterv1.GreetRequest, greeterv1.GreetResponse]
}

var _ greeterv1connect.GreeterServiceHandler = (*server)(nil)

func (s *server) Greet(ctx context.Context, req *connect.Request[greeterv1.GreetRequest]) (*connect.Response[greeterv1.GreetResponse], error) {
	return s.greet(ctx, req)
}

// NewGreeterHandler adapts svc to the generated Connect handler interface:
// one endpoint per method, each wrapped by mws and turned into a handler by
// connectrpc.NewConnectServer with its decoder and encoder. A client from
// client/greeter is a Service too, so a handler can be backed by another
// server, which makes a gateway.
func NewGreeterHandler(svc Service, mws ...kitendpoint.Middleware) greeterv1connect.GreeterServiceHandler {
	eps := MakeEndpoints(svc, mws...)
	return &server{
		greet: connectrpc.NewConnectServer(eps.Greet, decodeGreetRequest, encodeGreetResponse),
	}
}

// decodeGreetRequest turns the wire message into the business request. A
// failure here reaches the client as CodeInvalidArgument.
func decodeGreetRequest(_ context.Context, msg *greeterv1.GreetRequest) (*GreetRequest, error) {
	if msg.GetName() == "" {
		return nil, errors.New("name is required")
	}
	return &GreetRequest{Name: msg.GetName()}, nil
}

func encodeGreetResponse(_ context.Context, resp *GreetResponse) (*greeterv1.GreetResponse, error) {
	return &greeterv1.GreetResponse{Greeting: resp.Greeting}, nil
}
