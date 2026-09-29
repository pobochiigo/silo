package greeter

import (
	"context"

	"connectrpc.com/connect"

	"github.com/pobochiigo/silo/connectrpc"

	greeterv1 "github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1"
	"github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1/greeterv1connect"
	bizgreeter "github.com/pobochiigo/silo/examples/connectrpc/greeter"
)

// NewGreeterClient returns a Service that calls the greeter server at baseURL
// through the generated Connect client: one typed endpoint per method, built
// by connectrpc.NewConnectClient with its encoder and decoder.
func NewGreeterClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) bizgreeter.Service {
	client := greeterv1connect.NewGreeterServiceClient(httpClient, baseURL, opts...)
	return &endpoints{
		greet: connectrpc.NewConnectClient(client.Greet, encodeGreetRequest, decodeGreetResponse),
	}
}

func encodeGreetRequest(_ context.Context, req *bizgreeter.GreetRequest) (*greeterv1.GreetRequest, error) {
	return &greeterv1.GreetRequest{Name: req.Name}, nil
}

func decodeGreetResponse(_ context.Context, msg *greeterv1.GreetResponse) (*bizgreeter.GreetResponse, error) {
	return &bizgreeter.GreetResponse{Greeting: msg.GetGreeting()}, nil
}
