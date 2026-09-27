# Silo: Standalone Unit of Work, Telemetry, and Middleware Generator

Silo is a core library containing a transactional Unit of Work engine, OpenTelemetry integrations, and a middleware decorator generator for standard Go interfaces.

## Features

- **Unit of Work (UoW)**: A driver-agnostic transaction coordinator that queues tasks to execute in a single database transaction, with configurable retries and isolation levels. Out-of-the-box support for Go standard library `database/sql`, `sqlx` (including named queries), and native `pgx` (`pgxpool.Pool` and `pgx.Tx`).
- **Telemetry**: OpenTelemetry bootstrappers for traces, metrics, and logs, alongside `go-kit` endpoint middlewares.
- **Middlegen**: A command-line tool that parses Go interfaces and automatically generates production-ready middleware wrappers for logging, tracing, metrics, and UoW boundaries.

---

## Installation

```bash
go get github.com/pobochiigo/silo
```

To install the middleware generator CLI:
```bash
go install github.com/pobochiigo/silo/cmd/middlegen@latest
```

---

## Directory Structure

```
silo/
├── cmd/
│   └── middlegen/       # Middleware decorator generator CLI
├── connectrpc/          # ConnectRPC adapters for type-safe endpoints
├── db/                  # SQL, sqlx and pgx database transaction adapters
├── endpoint/            # Generic, type-safe endpoint signature
├── middleware/          # Generic middleware type definitions
├── telemetry/           # OpenTelemetry trackers, exporters, and middlewares
└── uow/                 # Core Unit of Work transaction orchestrator
```

---

## Architecture Overview

```mermaid
graph TD
    Service[Service Layer] -->|Uses| UoW[uow.Manager]
    UoW -->|Wraps in Transaction| DB[db.SQLTransactor / db.PGXTransactor]
    Service -->|Decorated by| Middleware[Generated Middlewares]
    Middleware -->|Publishes| Telemetry[telemetry.Metrics / Tracing]
```

---

## The transaction model

`uow.Manager` offers two entry points. Pick deliberately: they differ in what the transaction covers.

### `RunWith`: deferred writes

1. The business action runs **first, outside any transaction**, with a context carrying a fresh `uow.UnitOfWork`. Repository reads made here go straight to the connection pool.
2. Repository methods decorated with the `uow_repo` middleware do not execute; they **queue** themselves on the unit of work and return immediately (see [What deferred methods return](#what-deferred-methods-return)).
3. When the action returns `nil`, a transaction is opened and the queued tasks run inside it, in order, followed by a commit. No transaction is opened when nothing was queued.
4. If a task or the commit fails with an error the retry evaluator accepts, the **queued tasks** are re-run in a fresh transaction. The action is not re-run, so the closures execute with the values they captured in step 1.

The transaction, its isolation level and the retries therefore cover the deferred writes only. Reads made in the action are not isolated, and a read-modify-write flow can act on data that changed between the read and the commit.

### `RunInTx`: everything inside the transaction

1. The transaction is opened **first**. The action runs with a context carrying both the transaction and a fresh unit of work, so repository reads (and any immediate writes) execute inside the transaction under its isolation level.
2. Deferred tasks run in the same transaction after the action returns `nil`, then the transaction is committed.
3. On a retryable error the **whole action** is re-run in a fresh transaction. The action must therefore be safe to repeat with respect to side effects outside the database.

Use `RunInTx` for read-modify-write logic that must be isolated; use `RunWith` when the action is pure computation plus writes and you want to avoid holding a transaction open.

### Nesting and multiple databases

A nested `RunWith`/`RunInTx` call (a context that already carries a unit of work) joins the outer boundary: it runs immediately and its deferred tasks are committed by the outer call. The `db` package stores one active transaction per context, so nesting boundaries that belong to **different databases** is not supported: the inner tasks would run against the outer transaction.

### Retrying serialization failures

```go
transactor := db.NewPGXTransactor(pool, db.WithPGXTxOptions(pgx.TxOptions{IsoLevel: pgx.Serializable}))
manager := uow.NewManager(transactor,
    uow.WithRetryEvaluator(db.IsRetryableTxError), // SQLSTATE 40001 and 40P01
    uow.WithMaxRetries(3),
    uow.WithRetryDelay(50*time.Millisecond, time.Second),
)
```

`db.IsRetryableTxError` recognises PostgreSQL `serialization_failure` (40001) and `deadlock_detected` (40P01) through any driver whose errors expose `SQLState()`, which includes pgx and lib/pq. Retries back off exponentially from the base delay up to the cap, minus up to 25% random jitter so colliding workers do not retry in lock-step.

---

## Comprehensive End-to-End Walkthrough

Below is a complete implementation walkthrough demonstrating how to define database repositories, build a service layer, annotate them for `middlegen`, and wire everything up using a native PostgreSQL `pgx` connection.

### Step 1: Define the Repository & Annotate for `middlegen`

Write your repository interface and model. We annotate it with `//go:generate middlegen` to automatically generate our Unit of Work repository decorator (`uow_repo`), logger, and tracers.

We annotate query methods with `//middlegen:non-transactional` so they execute immediately without being queued in the transaction queue.

`db/user_repository.go`:
```go
package db

import (
	"context"

	"github.com/pobochiigo/silo/db"
)

type User struct {
	ID   string
	Name string
}

//go:generate middlegen -type=UserRepository -kinds=uow_repo,logging,tracing
type UserRepository interface {
	//middlegen:non-transactional
	GetByID(ctx context.Context, id string) (*User, error)

	Save(ctx context.Context, user *User) (*User, error)
}

type postgresUserRepository struct {
	pool db.PGXCommon
}

func NewUserRepository(pool db.PGXCommon) UserRepository {
	return &postgresUserRepository{pool: pool}
}

func (r *postgresUserRepository) GetByID(ctx context.Context, id string) (*User, error) {
	// db.PGXExecutor retrieves the active pgx.Tx transaction from the context if it exists,
	// or falls back to the connection pool/client.
	executor := db.PGXExecutor(ctx, r.pool)

	var user User
	err := executor.QueryRow(ctx, "SELECT id, name FROM users WHERE id=$1", id).Scan(&user.ID, &user.Name)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (r *postgresUserRepository) Save(ctx context.Context, user *User) (*User, error) {
	executor := db.PGXExecutor(ctx, r.pool)

	_, err := executor.Exec(ctx, "INSERT INTO users (id, name) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET name=$2", user.ID, user.Name)
	return user, err
}
```

### Step 2: Define the Service & Annotate for `middlegen`

The service layer orchestrates business logic and manages the Unit of Work lifecycle boundaries. We use the `uow_service` kind to auto-wrap service execution in `uow.Manager.RunWith` boundaries: writes queued by the repository are committed in one transaction when the service method returns, with automatic retries of transient failures.

`service/user_service.go`:
```go
package service

import (
	"context"

	mydb "my-app/db"
)

//go:generate middlegen -type=UserService -kinds=uow_service,logging,tracing
type UserService interface {
	CreateUser(ctx context.Context, id string, name string) error
}

type userService struct {
	repo mydb.UserRepository
}

func NewUserService(repo mydb.UserRepository) UserService {
	return &userService{repo: repo}
}

func (s *userService) CreateUser(ctx context.Context, id string, name string) error {
	user := &mydb.User{ID: id, Name: name}

	// With the uow_repo middleware, Save queues the database write and hands
	// `user` straight back: it is the source of truth until the commit.
	saved, err := s.repo.Save(ctx, user)
	if err != nil {
		return err
	}
	_ = saved // == user while deferred
	return nil
}
```

### Step 3: Generate the Middlewares

Run Go generate from your shell:
```bash
go generate ./...
```
This automatically produces the following decorators inside your package directories:
- `user_repository_logging_middleware.gen.go`
- `user_repository_tracing_middleware.gen.go`
- `user_repository_uow_middleware.gen.go`
- `user_service_logging_middleware.gen.go`
- `user_service_tracing_middleware.gen.go`
- `user_service_uow_middleware.gen.go`

### Step 4: Wire Everything Up in `main.go`

Configure telemetry and initialize database pools. Decorate your concrete structs with the generated chainable middlewares.

`main.go`:
```go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pobochiigo/silo/db"
	"github.com/pobochiigo/silo/telemetry"
	"github.com/pobochiigo/silo/uow"

	mydb "my-app/db"
	mysvc "my-app/service"
)

func main() {
	ctx := context.Background()

	// 1. Bootstrap Telemetry (traces, metrics, and logs over OTLP gRPC)
	shutdown, err := telemetry.InitTelemetry(ctx, telemetry.Config{
		ServiceName:    "user-service",
		ServiceVersion: "1.0.0",
		Environment:    "dev",
		Endpoint:       "localhost:4317",
		Insecure:       true, // plaintext gRPC; omit for TLS with system roots
	})
	if err != nil {
		slog.Error("Failed to bootstrap telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdown(ctx)

	// 2. Initialize Database Pool
	pool, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5432/postgres")
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// 3. Create Unit of Work Manager
	transactor := db.NewPGXTransactor(pool)
	uowManager := uow.NewManager(transactor, uow.WithRetryEvaluator(db.IsRetryableTxError))

	// 4. Instantiate and Decorate the Repository
	rawRepo := mydb.NewUserRepository(pool)

	// Apply Repository decorators (ordering: innermost is raw implementation)
	repo := mydb.UserRepositoryUoWMiddleware()(rawRepo)
	repo = mydb.UserRepositoryLoggingMiddleware()(repo)
	repo = mydb.UserRepositoryTracingMiddleware()(repo)

	// 5. Instantiate and Decorate the Service
	rawSvc := mysvc.NewUserService(repo)

	// Apply Service decorators
	svc := mysvc.UserServiceUoWMiddleware(uowManager)(rawSvc)
	svc = mysvc.UserServiceLoggingMiddleware()(svc)
	svc = mysvc.UserServiceTracingMiddleware()(svc)

	// 6. Invoke your decorated service
	err = svc.CreateUser(ctx, "user-123", "Alice")
	if err != nil {
		slog.Error("CreateUser execution failed", "error", err)
		return
	}
	slog.Info("CreateUser successfully executed inside a transaction with full tracing & logging!")
}
```

---

## Choosing a Transactor

The `db` package ships three `uow.Transactor` adapters. Pick the one matching how your repositories execute queries:

| Transactor | Injects into context | Use when |
|---|---|---|
| `db.NewSQLTransactor(db)` | `*sql.Tx` | Repositories use plain `database/sql` via `db.Executor`. |
| `db.NewSQLXTransactor(sqlxDB)` | `*sqlx.Tx` | Repositories use `sqlx` via `db.XExecutor` — required for named queries (`NamedExecContext`, `Rebind`), which need the driver's bindvar type. |
| `db.NewPGXTransactor(pool)` | `pgx.Tx` | Repositories use native `pgx` via `db.PGXExecutor`. |

> **Note:** `db.XExecutor` can also wrap a plain `*sql.Tx` (begun by `SQLTransactor`) on the fly, inheriting the name mapper of a `*sqlx.DB` fallback (or sqlx's default mapper otherwise), but that wrapper has no driver name, so named queries would render `?` placeholders and fail on Postgres. Use `SQLXTransactor` if you need named queries inside transactions. `XExecutor` panics if the context carries an executor it cannot adapt, rather than silently running your statements outside the transaction.

Isolation levels and access modes can be set per transactor:

```go
sqlTx  := db.NewSQLTransactor(pool, db.WithSQLTxOptions(&sql.TxOptions{Isolation: sql.LevelSerializable}))
sqlxTx := db.NewSQLXTransactor(sqlxDB, db.WithSQLXTxOptions(&sql.TxOptions{Isolation: sql.LevelRepeatableRead}))
pgxTx  := db.NewPGXTransactor(pgxPool, db.WithPGXTxOptions(pgx.TxOptions{IsoLevel: pgx.Serializable}))
```

`WithPGXTxOptions` needs a pool that implements `BeginTx(ctx, pgx.TxOptions)` such as `*pgxpool.Pool`; `NewPGXTransactor` panics at construction otherwise, so the misconfiguration surfaces at startup rather than on the first request.

---

## `middlegen` CLI Reference

### Command Line Flags

| Flag | Default | Description |
|---|---|---|
| `-type` | *(Required)* | Target interface name to generate middlewares for (e.g. `UserRepository`). |
| `-kinds` | `logging,tracing,metrics` | Comma-separated middlewares to generate (`logging`, `tracing`, `metrics`, `uow_repo`, `uow_service`). |
| `-service` | *(Inferred)* | The telemetry service prefix/name. Defaults to package name. |
| `-dir` | `""` | Directory relative to module root where the interface is declared. |
| `-prefix` | `middlegen` | Directive prefix namespace for comment annotations. |
| `-middleware-import` | *(Inferred)* | Import path of the generic `Middleware` helper package. |
| `-middleware-type` | `middleware.Middleware` | The type signature representation for middlewares. |
| `-library-module` | `github.com/pobochiigo/silo` | Module path providing the `middleware`/`telemetry`/`uow` packages referenced by generated code. |

### Directives

Directives are comments placed on interface methods (doc comment or trailing line comment), prefixed with `-prefix`:

| Directive | Applies to | Effect |
|---|---|---|
| `//middlegen:non-transactional` | `uow_repo` | Run the method immediately even inside a unit of work. Use it for reads. |
| `//middlegen:echo <param>[, <param>...]` | `uow_repo` | Return the named parameters, in order, as the deferred method's non-error results. `echo none` disables echoing. |
| `//middlegen:redact <param>[, <param>...]` | `logging` | Log the named parameters as `[REDACTED]`. |
| `//middlegen:metric attr:<name>=<expr>` | `metrics` | Add a metric attribute computed from a Go expression over the parameters. |
| `//middlegen:metric counter:<name>` | `metrics` | Increment a custom counter on every call. |

> **Cardinality warning:** metric attributes become label values on every series. Never derive them from unbounded inputs such as user or order IDs.

### What deferred methods return

A `uow_repo` method that runs inside a unit of work is queued, not executed, so it has to return *something* immediately. Silo hands the caller back the object it passed in, which is the source of truth for a write that has not happened yet:

- `//middlegen:echo` decides explicitly which parameters are returned.
- Otherwise, a result is echoed when **exactly one** parameter has its type (a `*T` parameter also satisfies a `T` result, guarded against `nil`, and vice versa) and that type is not a basic type (`string`, `int`, `bool`, ...). `Save(ctx, user *User) (*User, error)` returns `user`.
- Every other result is its zero value. Basic-typed results are never echoed from parameters; when several parameters are candidates, the generator warns and returns the zero value until you add an `echo` directive.

Read methods must be annotated `//middlegen:non-transactional` so they execute immediately and return real data.

### Notes on generated code

- `middlegen` type-checks the package with `go/packages`, so it needs the `go` tool and resolvable module dependencies. Type errors caused by not-yet-generated code are reported and tolerated; errors in the interface's own signatures abort generation.
- Methods of **embedded interfaces** are decorated like any other, whether the embedded interface lives in the same package, another package of your module, a dependency, or the standard library (`io.Closer`), and however deeply the embeddings nest. Directives written on the embedded interface's methods apply wherever it is embedded. The one exception is an unexported method declared in another package: Go does not allow implementing it from outside, so it is forwarded through the embedded field and a warning names it.
- Parameters whose names collide with identifiers used by the templates (`t`, `m`, `err`, `ok`, `span`, the packages the templates import such as `time` or `uow`, and the qualifiers of packages your signatures use) are transparently renamed in the generated code; log attribute keys and `metric attr` expressions keep the original names. Blank (`_`) and unnamed parameters become `p0`, `p1`, .... The context parameter may appear at any position.
- Types from other packages are qualified from type information, so a package imported under an alias in your file, two packages sharing a name, or a package whose name differs from its path's last element (`pgx/v5`) are all imported correctly in the generated files.
- The logging middleware logs the `<Method> started` line with all parameters at `Debug` level and failures at `Error` level. Redact secrets with `//middlegen:redact`.
- A `uow_service` method that returns no `error` cannot report a failed commit; the generated wrapper logs the failure through `slog.Default()` and the generator prints a warning naming the method. Prefer returning an error.
- Methods without a `context.Context` are still logged (without trace correlation) and measured; tracing needs a context to start a span, and Unit of Work boundaries need one to find the unit, so those wrappers forward such methods unchanged.
- Generic interfaces (type parameters) are not supported; the generator refuses them with a clear message.

---

## Type-Safe Endpoints & ConnectRPC Integration

Silo provides a type-safe generic endpoint abstraction and adapters for seamless integration with ConnectRPC.

### Type-Safe Endpoint
Defined in the `endpoint` package, it offers a generic signature for service endpoints, avoiding `interface{}`-based wrappers:
```go
package endpoint

import "context"

type Endpoint[Req any, Resp any] func(ctx context.Context, request Req) (Resp, error)
```

### ConnectRPC Adapters
The `connectrpc` package adapts these type-safe endpoints to ConnectRPC server handlers and client endpoints. The endpoint sees only the decoded message: request headers, response headers and trailers are not exposed. Handle them in a Connect interceptor, or read them in the decoder, which receives the raw `*connect.Request`'s message and context.

#### Server Handler Construction (`NewConnectServer`)
Converts a generic `endpoint.Endpoint` into a ConnectRPC server handler, using custom decoders and encoders. Decode failures are reported as `CodeInvalidArgument` and encode failures as `CodeInternal` unless the error already carries a Connect code; endpoint errors pass through untouched.
```go
import (
	"github.com/pobochiigo/silo/connectrpc"
	"github.com/pobochiigo/silo/endpoint"
)

handler := connectrpc.NewConnectServer(
	endpoint,
	func(ctx context.Context, protoReq *pb.MyRequest) (MyRequest, error) {
		// decode proto message to domain request
		return MyRequest{Name: protoReq.Name}, nil
	},
	func(ctx context.Context, resp MyResponse) (*pb.MyResponse, error) {
		// encode domain response to proto message
		return &pb.MyResponse{Id: resp.ID}, nil
	},
)
```

#### Client Endpoint Construction (`NewConnectClient`)
Wraps a ConnectRPC client call inside a type-safe `endpoint.Endpoint`:
```go
clientEndpoint := connectrpc.NewConnectClient(
	client.MyMethod,
	func(ctx context.Context, req MyRequest) (*pb.MyRequest, error) {
		// encode domain request to proto request
		return &pb.MyRequest{Name: req.Name}, nil
	},
	func(ctx context.Context, protoResp *pb.MyResponse) (MyResponse, error) {
		// decode proto response to domain response
		return MyResponse{ID: protoResp.Id}, nil
	},
)
```

---

## Advanced Telemetry Features

In addition to bootstrapping OpenTelemetry traces, metrics, and logs, the `telemetry` package provides utilities for integrating with legacy frameworks and managing context propagation.

### Configuration

`telemetry.Config` fields:

| Field | Description |
|---|---|
| `ServiceName`, `ServiceVersion`, `Environment` | Resource attributes attached to all traces, metrics, and logs. The environment is emitted as both `deployment.environment.name` (current semantic conventions) and `deployment.environment`. Empty values are left out so `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` can fill them; non-empty values win over the environment. |
| `Endpoint` | `host:port` of the OTLP gRPC collector (Grafana Alloy, OTel Collector, ...). When empty, the exporters use `OTEL_EXPORTER_OTLP_ENDPOINT` and then the OTLP default `localhost:4317`. |
| `Insecure` | Disables TLS on exporter connections (plaintext gRPC). Defaults to `false` — TLS with the system certificate pool. |
| `Headers` | Extra gRPC metadata sent with every export, e.g. collector auth tokens. |
| `SkipSlogDefault` | Prevents `InitLogs` from replacing the process-wide `slog` default logger. |
| `LocalLogHandler` | Handler that receives every record in addition to the OTLP exporter. Defaults to a text handler on stderr. |
| `DisableLocalLogs` | Send logs to the collector only. Note that `slog.SetDefault` also routes the standard `log` package through slog, so nothing is written locally. |

The resource also carries the `telemetry.sdk.*` attributes and the semantic-conventions schema URL.

`InitTraces` registers the W3C `TraceContext`/`Baggage` propagators globally. Applications that skip tracing but still forward trace headers can call `telemetry.InitPropagators()` directly.

### Logs

`InitLogs` installs a default `slog` logger that fans every record out to a local handler (stderr by default, or `LocalLogHandler`) **and** to the OTLP exporter through the official OTel bridge, so log lines carry the active trace and span IDs without disappearing from the machine when the collector is unreachable. `telemetry.NewFanoutHandler` is exported for building your own combinations.

### Go-Kit Endpoint Middlewares
Standard endpoint middlewares designed to wrap Go-Kit (`github.com/go-kit/kit/endpoint`) endpoints:
- `MetricsMiddleware(operationName)`: Automatically records execution counts and latency durations via OTel metrics.
- `LoggingMiddleware(operationName, logger)`: Logs execution status, elapsed time, and errors via `slog` (TraceID-correlated).
- `TracingMiddleware(operationName)`: Automatically creates child tracing spans around endpoint execution.

### Go-Kit Log Compatibility (`SlogAdapter`)
Bridges the gap between legacy `go-kit/log.Logger` interfaces and modern standard `log/slog`. Route go-kit logs directly through your globally registered OTel bridge:
```go
import "github.com/pobochiigo/silo/telemetry"

// Instantiates a go-kit Logger adapter mapping keyvals to structured slog
logger := telemetry.NewSlogAdapter(ctx)
```

### Trace Context Propagation
Injects and extracts trace contexts between Go `context.Context` and transport network layers (HTTP Headers and gRPC Metadata):
- `ExtractHTTPTraceContext()`: HTTP server request function to parse incoming tracing headers.
- `InjectHTTPTraceContext()`: HTTP client request function to inject outgoing tracing headers.
- `ExtractGRPCTraceContext()`: gRPC server request handler to extract trace information from incoming metadata.
- `InjectGRPCTraceContext()`: gRPC client request handler to inject trace information into outgoing metadata.
