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
| `greeter/endpoint.go` | `Endpoints`, one exported `endpoint.Endpoint[*GreetRequest, *GreetResponse]` per method, and `MakeEndpoints(svc)`. Callers decorate the fields before handing them to the transport. |
| `greeter/connectrpc_server.go` | `NewGreeterHandler(eps)`: the transport layer, the generated handler interface implemented by delegating to `connectrpc.NewConnectServer` handlers, with the decoder and encoder of each method. |
| `client/greeter/endpoint.go` | An unexported `endpoints` struct that implements `greeter.Service` by calling one endpoint per method. |
| `client/greeter/connectrpc_transport.go` | `NewGreeterClient(httpClient, baseURL, opts...) greeter.Service`: the generated Connect client turned into typed endpoints by `connectrpc.NewConnectClient`, with the encoder and decoder of each method. |
| `main.go` | Wires the server, the client and a gateway, and walks through the steps below. |

The two packages depend on `silo/endpoint` and `silo/connectrpc` only, which
pull nothing beyond `connectrpc.com/connect` into a consumer's module graph.
Observability is wired by the caller, in `main.go`.

## What the run shows

1. **Server.** Three layers. `middleware.Chain` wraps `greeter.NewService()`
   in the generated logging and tracing decorators. `greeter.MakeEndpoints`
   turns the `Service` into typed endpoints, and `telemetry.Metrics`
   decorates the `Greet` field, recording on a `MetricsRecorder` whose
   subsystem, `greeter_endpoint`, names the series.
   `greeter.NewGreeterHandler(eps)` turns the endpoints into the generated
   handler interface through `connectrpc.NewConnectServer`, with the decoder
   and encoder of each method. `decodeGreetRequest` rejects an empty name;
   the service answers `CodeNotFound` for "nobody".
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
   `greeter.NewGreeterHandler(greeter.MakeEndpoints(greeterclient.NewGreeterClient(...)))`
   serves the upstream server through a second one. The gateway needs no
   code of its own, and `CodeNotFound` survives both hops.
6. **Metrics.** `greeter_endpoint_requests_total`,
   `greeter_endpoint_errors_total` and
   `greeter_endpoint_request_duration_seconds` from the endpoint middleware
   of the upstream server, labelled with `method`.

```
== 5. Gateway: a client is a Service, so a handler can be backed by another server; codes survive both hops
   [span] greeter.Greet              trace=2aa45ba5584f1766fe86f0fff1aba200      2µs  Unset
   "Hello, Grace" through the gateway
   [span] greeter.Greet              trace=2da6437a1bcaf71a126b5daca35fffdb      5µs  Error: not_found: greeter: unknown person
time=2026-09-29T05:39:33.610Z level=ERROR msg="Greet failed" service=greeter error="not_found: greeter: unknown person"
   code=not_found err=not_found: greeter: unknown person

== 6. Metrics recorded by the endpoint middleware of the upstream server
   greeter_endpoint_requests_total{method=Greet} = 4
   greeter_endpoint_errors_total{method=Greet} = 2
   greeter_endpoint_request_duration_seconds{method=Greet} count=4
```

## Things worth copying

- The `Service` interface is the pivot. The server consumes it, the client
  produces it, and everything written against it (decorators, tests, a
  gateway) works on both sides.
- Decorate at the `Service` level with the generated middlewares and at the
  endpoint level with `telemetry.Tracing`, `telemetry.Logging` and
  `telemetry.Metrics` on the exported `Endpoints` fields; `middleware.Chain`
  composes either kind. A go-kit middleware fits an endpoint through
  `telemetry.Adapt`. Generated SDK code can spell out the type arguments per
  endpoint; a `NewXHandler(svc)` convenience that calls `MakeEndpoints`
  itself, as bhole's generators emit, still fits on top.
- Keep protobuf out of the domain: the codecs in `connectrpc_server.go` and
  `connectrpc_transport.go` are the only files that import `gen/`.
- Headers and trailers are not visible to the endpoint; handle them in a
  Connect interceptor, or read them in the decoder, which receives the
  message.
