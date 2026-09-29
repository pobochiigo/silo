# connectrpc: an SDK layout on typed endpoints

A `GreeterService` defined in protobuf, laid out the way a service SDK built
on silo's `endpoint` and `connectrpc` packages is: one package per feature
holding the domain types, the `Service` interface, its endpoints and the
Connect handler, and a client package whose constructor returns that same
`Service` backed by Connect calls. Server, client, a gateway made of the two,
and the generated decorators on both sides run in one process.

```bash
go run ./connectrpc
go run ./connectrpc -v       # Debug logging shows every "Greet started" line
```

## Layout

| Path | What it is |
|---|---|
| `proto/greeter/v1/greeter.proto` | The service definition. |
| `buf.yaml`, `buf.gen.yaml` | buf configuration; `buf generate` (with `protoc-gen-go` and `protoc-gen-connect-go` on `PATH`) writes `gen/`. |
| `gen/greeter/v1/` | Generated protobuf messages and the Connect client and handler interfaces. Committed, so nothing but Go is needed to build and run. |
| `greeter/greeter.go` | The domain types: `GreetRequest`, `GreetResponse`, `ErrUnknownPerson`. No protobuf, no Connect. |
| `greeter/service.go` | The `Service` interface, the `go:generate` line for its decorators, and the in-process implementation. |
| `greeter/*.gen.go` | Generated: `logging` and `tracing` for `Service`. They wrap the implementation on the server and the client on the caller's side. |
| `greeter/endpoint.go` | `Endpoints`, one `endpoint.Endpoint[*GreetRequest, *GreetResponse]` per method, and `MakeEndpoints(svc, mws...)`, which wraps every endpoint with the given go-kit middlewares through `telemetry.Adapt`. |
| `greeter/connectrpc_server.go` | `NewGreeterHandler(svc, mws...)`: the generated handler interface implemented by delegating to `connectrpc.NewConnectServer` handlers, with the decoder and encoder of each method. |
| `client/greeter/endpoint.go` | An unexported `endpoints` struct that implements `greeter.Service` by calling one endpoint per method. |
| `client/greeter/connectrpc_transport.go` | `NewGreeterClient(httpClient, baseURL, opts...) greeter.Service`: the generated Connect client turned into typed endpoints by `connectrpc.NewConnectClient`, with the encoder and decoder of each method. |
| `main.go` | Wires the server, the client and a gateway, and walks through the steps below. |

The two packages depend on `silo/endpoint` and `silo/connectrpc`, which pull
nothing beyond `connectrpc.com/connect` into a consumer's module graph;
`greeter/endpoint.go` adds go-kit only for the optional middleware parameter.

## What the run shows

1. **Server.** `greeter.NewService()` is wrapped by the generated tracing and
   logging middlewares, then `greeter.NewGreeterHandler(svc,
   telemetry.MetricsMiddleware("greeter"))` turns it into the generated
   handler interface: one endpoint per method, the metrics middleware on
   every endpoint through `telemetry.Adapt`, `connectrpc.NewConnectServer`
   around each with its decoder and encoder. `decodeGreetRequest` rejects an
   empty name; the service answers `CodeNotFound` for "nobody".
2. **Client.** `greeterclient.NewGreeterClient(http.DefaultClient, url)`
   returns a `greeter.Service`, so the same generated decorators wrap it. A
   call therefore produces two `greeter.Greet` spans, the client's and the
   server's, in separate traces; a Connect interceptor that propagates the
   trace context (such as `otelconnect`) would make one the parent of the
   other.
3. **Decode failure.** The client receives `CodeInvalidArgument` with the
   decoder's message: decode errors on the server are mapped to that code
   unless they already carry a Connect code. Only the client's span and log
   line appear, since the service never ran.
4. **Endpoint error.** The client receives `CodeNotFound`: endpoint errors
   pass through the adapter untouched, so the business code stays in charge
   of its codes. Both sides log the failure.
5. **Gateway.** A client is a `Service`, so
   `greeter.NewGreeterHandler(greeterclient.NewGreeterClient(...))` serves
   the upstream server through a second one. The gateway needs no code of
   its own, and `CodeNotFound` survives both hops.
6. **Metrics.** `gokit_requests_total` and `gokit_request_duration_seconds`
   from the endpoint middleware of the upstream server, labelled with
   `operation` and `success`.

```
== 5. Gateway: a client is a Service, so a handler can be backed by another server; codes survive both hops
   [span] greeter.Greet              trace=2aa45ba5584f1766fe86f0fff1aba200      2µs  Unset
   "Hello, Grace" through the gateway
   [span] greeter.Greet              trace=2da6437a1bcaf71a126b5daca35fffdb      5µs  Error: not_found: greeter: unknown person
time=2026-09-29T05:39:33.610Z level=ERROR msg="Greet failed" service=greeter error="not_found: greeter: unknown person"
   code=not_found err=not_found: greeter: unknown person

== 6. Metrics recorded by the endpoint middleware of the upstream server
   gokit_requests_total{operation=greeter,success=true} = 2
   gokit_requests_total{operation=greeter,success=false} = 2
```

## Things worth copying

- The `Service` interface is the pivot. The server consumes it, the client
  produces it, and everything written against it (decorators, tests, a
  gateway) works on both sides.
- Decorate at the `Service` level with the generated middlewares, and at the
  endpoint level with go-kit middlewares through `MakeEndpoints`. The
  first sees business types, the second sees every method alike.
- Keep protobuf out of the domain: the codecs in `connectrpc_server.go` and
  `connectrpc_transport.go` are the only files that import `gen/`.
- Headers and trailers are not visible to the endpoint; handle them in a
  Connect interceptor, or read them in the decoder, which receives the
  message.
