# connectrpc: type-safe endpoints over Connect

A `GreeterService` defined in protobuf, served by a business endpoint that
knows nothing about protobuf or Connect, and called through a typed client
endpoint. Server and client run in one process.

```bash
go run ./connectrpc
```

## Layout

| Path | What it is |
|---|---|
| `proto/greeter/v1/greeter.proto` | The service definition. |
| `buf.yaml`, `buf.gen.yaml` | buf configuration; `buf generate` (with `protoc-gen-go` and `protoc-gen-connect-go` on `PATH`) writes `gen/`. |
| `gen/greeter/v1/` | Generated protobuf messages and the Connect client and handler interfaces. Committed, so nothing but Go is needed to build and run. |
| `greeter.go` | The business request and response types, the endpoint, the four codec functions, and a hand-written middleware for typed endpoints. |
| `main.go` | Wires the server and the client and walks through the steps below. |

## What the run shows

1. **Server.** `connectrpc.NewConnectServer(greet, decodeGreet, encodeGreet)`
   returns a function with the signature of the generated handler method, so
   the handler struct just delegates to it. `decodeGreet` rejects an empty
   name; `greet` answers `CodeNotFound` for "nobody".
2. **Client.** `connectrpc.NewConnectClient(client.Greet, encodeGreetRequest,
   decodeGreetResponse)` turns the generated Connect client method into an
   `endpoint.Endpoint[GreetRequest, GreetResponse]`. It is decorated twice:
   `telemetry.Adapt` applies `telemetry.LoggingMiddleware`, a go-kit endpoint
   middleware, to the typed endpoint, and the hand-written `timing`
   middleware in `greeter.go` shows that `middleware.Middleware[T]` works for
   endpoint types as well as for the generated interface decorators.
3. **Decode failure.** The client receives `CodeInvalidArgument` with the
   decoder's message: decode errors on the server are mapped to that code
   unless they already carry a Connect code.
4. **Endpoint error.** The client receives `CodeNotFound`: endpoint errors
   pass through the adapter untouched, so the business code stays in charge
   of its codes.

```
== 3. A decode failure on the server is reported as CodeInvalidArgument
   [client] 760µs err=invalid_argument: name is required
   code=invalid_argument err=invalid_argument: name is required

== 4. An endpoint error passes through with the code the endpoint chose
   [client] 351µs err=not_found: greeter: unknown person
   code=not_found err=not_found: greeter: unknown person
```

Headers and trailers are not visible to the endpoint; handle them in a
Connect interceptor, or read them in the decoder, which receives the message.
