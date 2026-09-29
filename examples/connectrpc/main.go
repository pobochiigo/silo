// Command connectrpc serves a Connect RPC and calls it, laid out the way an
// SDK on silo's typed endpoints is: the greeter package holds the domain
// types, the Service interface, its endpoints and the Connect handler; the
// client/greeter package returns the same Service backed by Connect calls.
// The protobuf and Connect code under gen/ is generated from proto/ with buf
// (see buf.gen.yaml) and committed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"

	"github.com/pobochiigo/silo/telemetry"

	greeterclient "github.com/pobochiigo/silo/examples/connectrpc/client/greeter"
	"github.com/pobochiigo/silo/examples/connectrpc/gen/greeter/v1/greeterv1connect"
	"github.com/pobochiigo/silo/examples/connectrpc/greeter"
	"github.com/pobochiigo/silo/examples/internal/demo"
)

func main() {
	verbose := flag.Bool("v", false, "debug logging: shows the generated middlewares' started lines")
	flag.Parse()

	ctx, stop := demo.Context()
	defer stop()
	shutdown := demo.Telemetry(ctx, "silo-example-connectrpc", *verbose)
	defer shutdown()

	demo.Step(1, "Server: a Service implementation, decorated by the generated middlewares, becomes a Connect handler through NewGreeterHandler")
	var svc greeter.Service = greeter.NewService()
	svc = greeter.ServiceTracingMiddleware()(svc)
	svc = greeter.ServiceLoggingMiddleware()(svc)
	// The go-kit metrics middleware wraps every endpoint through telemetry.Adapt.
	upstream := serve(greeter.NewGreeterHandler(svc, telemetry.MetricsMiddleware("greeter")))
	defer upstream.Close()
	fmt.Println("   serving", greeterv1connect.GreeterServiceName, "at", upstream.URL)

	demo.Step(2, "Client: NewGreeterClient returns the same Service interface, so the same decorators apply on this side")
	var client greeter.Service = greeterclient.NewGreeterClient(http.DefaultClient, upstream.URL)
	client = greeter.ServiceTracingMiddleware()(client)
	client = greeter.ServiceLoggingMiddleware()(client)
	resp, err := client.Greet(ctx, &greeter.GreetRequest{Name: "Ada"})
	if err != nil {
		demo.Fail("greet", err)
	}
	fmt.Printf("   %q\n", resp.Greeting)

	demo.Step(3, "A decode failure on the server is reported as CodeInvalidArgument")
	_, err = client.Greet(ctx, &greeter.GreetRequest{Name: ""})
	fmt.Printf("   code=%s err=%v\n", connect.CodeOf(err), err)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		demo.Fail("expected CodeInvalidArgument", err)
	}

	demo.Step(4, "An endpoint error passes through with the code the service chose")
	_, err = client.Greet(ctx, &greeter.GreetRequest{Name: "nobody"})
	fmt.Printf("   code=%s err=%v\n", connect.CodeOf(err), err)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeNotFound {
		demo.Fail("expected CodeNotFound", err)
	}

	demo.Step(5, "Gateway: a client is a Service, so a handler can be backed by another server; codes survive both hops")
	gateway := serve(greeter.NewGreeterHandler(greeterclient.NewGreeterClient(http.DefaultClient, upstream.URL)))
	defer gateway.Close()
	via := greeterclient.NewGreeterClient(http.DefaultClient, gateway.URL)
	resp, err = via.Greet(ctx, &greeter.GreetRequest{Name: "Grace"})
	if err != nil {
		demo.Fail("greet via gateway", err)
	}
	fmt.Printf("   %q through the gateway\n", resp.Greeting)
	_, err = via.Greet(ctx, &greeter.GreetRequest{Name: "nobody"})
	fmt.Printf("   code=%s err=%v\n", connect.CodeOf(err), err)
	if connect.CodeOf(err) != connect.CodeNotFound {
		demo.Fail("expected CodeNotFound through the gateway", err)
	}

	demo.Step(6, "Metrics recorded by the endpoint middleware of the upstream server")
	demo.PrintMetrics(ctx)
}

// serve mounts a handler on a test server.
func serve(h greeterv1connect.GreeterServiceHandler) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle(greeterv1connect.NewGreeterServiceHandler(h))
	return httptest.NewServer(mux)
}
