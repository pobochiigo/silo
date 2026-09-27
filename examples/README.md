# Silo examples

Runnable programs that show how the pieces of Silo fit together. They live in
their own Go module so that their dependencies never reach the library's
`go.mod`, and they build against the library in this repository through a
`replace` directive, so every change to Silo is exercised by them in CI.

| Example | Needs | Shows |
|---|---|---|
| [`middlegen/`](middlegen/) | nothing | Every `middlegen` directive on one interface, all four kinds of generated middleware, when deferred writes really run, `-dir` generation into another package, spans and metrics printed in-process. |
| [`uow/`](uow/) | PostgreSQL | One service and one set of generated middlewares on top of three repositories: database/sql, sqlx and pgx. `RunWith` deferral, `RunInTx` with `//middlegen:in-tx`, SERIALIZABLE retries under concurrency, the nesting rules. |
| [`telemetry/`](telemetry/) | nothing | `InitTelemetry`, the go-kit endpoint middlewares, trace propagation over HTTP headers and gRPC metadata, the go-kit log adapter, the fan-out log handler. |
| [`connectrpc/`](connectrpc/) | nothing | A Connect RPC served from a type-safe endpoint and called through a type-safe client endpoint, with the error code mapping of the adapters. |

## Running

Go 1.27.1 or newer is required. Every example runs with plain `go run`:

```bash
cd examples
go run ./middlegen
go run ./telemetry
go run ./connectrpc
```

The `uow` example needs a PostgreSQL database. `docker-compose.yml` starts one,
together with an OpenTelemetry Collector that prints everything it receives:

```bash
docker compose up -d
go run ./uow -driver pgx     # or -driver sql, -driver sqlx
```

Any other PostgreSQL works through `DATABASE_URL`
(default `postgres://postgres:postgres@localhost:5432/silo?sslmode=disable`).
The example creates its two tables and truncates them on every run.

Without `OTEL_EXPORTER_OTLP_ENDPOINT` the examples print finished spans to the
terminal and read their metrics back in-process. Point them at the collector
from the compose file to see the real pipeline, then read the collector's
output:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./uow
docker compose logs -f otel-collector
```

## How the examples use middlegen

The module declares the generator as a Go tool in `go.mod`
(`tool github.com/pobochiigo/silo/cmd/middlegen`), so the `go:generate` lines
read

```go
//go:generate go tool middlegen -type=Repository -kinds=uow_repo,logging,tracing,metrics -service=ledger
```

and `go generate ./...` regenerates every `*.gen.go` file with the generator
from this repository. CI runs that and fails on any difference, so the
committed files always match the templates. In your own project, do the same
against a released version:

```bash
go get -tool github.com/pobochiigo/silo/cmd/middlegen@latest
```

and drop the `replace` directive, which exists only to build against the
working tree.

The protobuf and Connect code of the `connectrpc` example is generated with
[buf](https://buf.build) from `connectrpc/proto` (`cd connectrpc && buf
generate`) and committed; Go and the CI job never need buf.

## Layout

```
examples/
├── docker-compose.yml     PostgreSQL 16 and an OTel Collector for local runs
├── otel-collector.yaml    collector config: OTLP in, debug exporter out
├── internal/demo/         shared helpers: env defaults, telemetry bootstrap, local span/metric printing
├── middlegen/             zero-infrastructure tour of the generator
├── uow/                   ledger service on database/sql, sqlx and pgx
├── telemetry/             go-kit HTTP endpoint with tracing, logging, metrics and propagation
└── connectrpc/            Connect RPC through the type-safe adapters
```
