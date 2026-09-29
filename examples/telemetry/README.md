# telemetry: endpoints, propagation, logs

A go-kit HTTP endpoint wrapped with Silo's endpoint middlewares, called by a
go-kit HTTP client, all in one process. No infrastructure is needed: spans
are printed as they end and the metrics are read back at the end. With
`OTEL_EXPORTER_OTLP_ENDPOINT` set, `telemetry.InitTelemetry` sends traces,
metrics and logs to the collector from `docker-compose.yml` instead.

```bash
go run ./telemetry
OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./telemetry   # with docker compose up -d
```

## What the run shows

1. **The middlewares.** The `greet` endpoint is wrapped with
   `telemetry.Metrics`, `telemetry.Logging` and `telemetry.Tracing`,
   instantiated with `[any, any]` and converted by `telemetry.Kit`, since
   go-kit's endpoint is `endpoint.Endpoint[any, any]` under another name. It
   is served by go-kit's HTTP transport with
   `httptransport.ServerBefore(telemetry.ExtractHTTPTraceContext())`, which
   turns the incoming `traceparent` header into the parent of the server span.
   Transport errors are reported through `telemetry.NewSlogAdapter`, so
   go-kit's own logging lands in slog.
2. **Propagation over HTTP.** The go-kit client sends the request with
   `httptransport.ClientBefore(telemetry.InjectHTTPTraceContext())`. The
   endpoint returns the trace ID it observed, and it is the client's:

   ```
      [span] greet                      trace=44c2d3b9a9d5f8e1957f6cccd7511ffa     84µs  Ok
      [span] greet.client               trace=44c2d3b9a9d5f8e1957f6cccd7511ffa  1.808ms  Ok
      [span] example.main               trace=44c2d3b9a9d5f8e1957f6cccd7511ffa  1.969ms  Unset
      trace id seen by the client: 44c2d3b9a9d5f8e1957f6cccd7511ffa
      trace id seen by the server: 44c2d3b9a9d5f8e1957f6cccd7511ffa
   ```
3. **An endpoint error.** The server span carries the error and its status,
   the log line is at Error level with the `error` attribute, and the error
   counter grows. go-kit answers with status 500 and the error
   text, which the client decoder hands back.
4. **Propagation over gRPC metadata.** `telemetry.InjectGRPCTraceContext`
   writes the active span into a `metadata.MD`, and
   `telemetry.ExtractGRPCTraceContext` reads it back on the callee side; the
   trace and parent span IDs survive the round trip. In a real service these
   are the go-kit gRPC transport's `ServerBefore`/`ClientBefore` functions.
5. **go-kit logging and the fan-out handler.** `telemetry.NewSlogAdapter`
   maps `level`, `msg` and `err` to slog and drops `ts`.
   `telemetry.NewFanoutHandler` sends every record to both the existing
   handler and a counting handler.
6. **Metrics.** `greet_endpoint_requests_total`, `greet_endpoint_errors_total`
   and `greet_endpoint_request_duration_seconds`, labelled with `method`. The
   recorder's subsystem, `greet_endpoint`, names them.

## Things worth copying

- `demo.Telemetry` in `internal/demo` is the bootstrap an application would
  write: `telemetry.InitTelemetry` with the endpoint from the environment,
  `Insecure` for a plaintext collector, and a deferred shutdown with its own
  timeout.
- Build the generated decorators, which capture `slog.Default()`, after
  `InitTelemetry`; `telemetry.Logging` with a nil logger and the slog
  adapter resolve the default lazily, so their order does not matter.
