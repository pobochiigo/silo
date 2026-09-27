// Command connectrpc serves a Connect RPC from a type-safe endpoint and calls
// it through a type-safe client endpoint, using the adapters in the
// connectrpc package. The protobuf and Connect code under gen/ is generated
// from proto/ with buf (see buf.gen.yaml) and committed.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"

	silorpc "github.com/pobochiigo/silo/connectrpc"

	greeterv1 "github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1"
	"github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1/greeterv1connect"
	"github.com/pobochiigo/silo/examples/internal/demo"
)

// greeterServer implements the generated handler interface by delegating to
// the adapted endpoint.
type greeterServer struct {
	greeterv1connect.UnimplementedGreeterServiceHandler
	greet silorpc.Handler[greeterv1.GreetRequest, greeterv1.GreetResponse]
}

func (s *greeterServer) Greet(ctx context.Context, req *connect.Request[greeterv1.GreetRequest]) (*connect.Response[greeterv1.GreetResponse], error) {
	return s.greet(ctx, req)
}

func main() {
	ctx := context.Background()

	demo.Step(1, "Server: the business endpoint becomes a Connect handler through NewConnectServer")
	server := &greeterServer{greet: silorpc.NewConnectServer(greet, decodeGreet, encodeGreet)}
	mux := http.NewServeMux()
	mux.Handle(greeterv1connect.NewGreeterServiceHandler(server))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	fmt.Println("   serving", greeterv1connect.GreeterServiceName, "at", ts.URL)

	demo.Step(2, "Client: the generated Connect client becomes a typed endpoint through NewConnectClient")
	client := greeterv1connect.NewGreeterServiceClient(http.DefaultClient, ts.URL)
	greetEndpoint := silorpc.NewConnectClient(client.Greet, encodeGreetRequest, decodeGreetResponse)
	greetEndpoint = timing[GreetRequest, GreetResponse]("client")(greetEndpoint)

	resp, err := greetEndpoint(ctx, GreetRequest{Name: "Ada"})
	if err != nil {
		demo.Fail("greet", err)
	}
	fmt.Printf("   %q\n", resp.Greeting)

	demo.Step(3, "A decode failure on the server is reported as CodeInvalidArgument")
	_, err = greetEndpoint(ctx, GreetRequest{Name: ""})
	fmt.Printf("   code=%s err=%v\n", connect.CodeOf(err), err)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		demo.Fail("expected CodeInvalidArgument", err)
	}

	demo.Step(4, "An endpoint error passes through with the code the endpoint chose")
	_, err = greetEndpoint(ctx, GreetRequest{Name: "nobody"})
	fmt.Printf("   code=%s err=%v\n", connect.CodeOf(err), err)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeNotFound {
		demo.Fail("expected CodeNotFound", err)
	}
}
