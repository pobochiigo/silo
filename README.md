# Silo: Standalone Unit of Work, Telemetry, and Middleware Generator

Silo is a core library containing a transactional Unit of Work engine, OpenTelemetry integrations, and a middleware decorator generator for standard Go interfaces.

## Features

- **Unit of Work (UoW)**: A driver-agnostic transaction coordinator that queues tasks to execute in a single database transaction, with configurable retries and isolation levels. Out-of-the-box support for Go standard library `database/sql`, `sqlx` (including named queries), and native `pgx` (`pgxpool.Pool` and `pgx.Tx`).
- **Telemetry**: OpenTelemetry bootstrappers for traces, metrics, and logs, alongside typed endpoint middlewares that fit go-kit endpoints as well.
- **Middlegen**: A command-line tool that parses Go interfaces and automatically generates production-ready middleware wrappers for logging, tracing, metrics, and UoW boundaries.

---

## Installation

Silo requires Go 1.27.1 or newer.

```bash
go get github.com/pobochiigo/silo
```

The middleware generator is a separate program whose output calls into the library, so keep the two at the same version. The simplest way is to record it as a Go tool in your `go.mod`:
```bash
go get -tool github.com/pobochiigo/silo/cmd/middlegen@latest
```
```go
//go:generate go tool middlegen -type=UserRepository -kinds=uow_repo,logging,tracing
```
`go tool` builds the generator from the version pinned in `go.mod`, so every developer and CI run uses the same one. Two alternatives: install a binary at the library version your module uses,
```bash
go install github.com/pobochiigo/silo/cmd/middlegen@$(go list -m -f '{{.Version}}' github.com/pobochiigo/silo)
```
or let `go generate` fetch a pinned version on demand:
```go
//go:generate go run github.com/pobochiigo/silo/cmd/middlegen@v0.3.0 -type=UserRepository -kinds=uow_repo,logging,tracing
```

---

## Examples

The [`examples`](examples/) directory is a separate Go module with runnable programs, built and run against the library in CI:

| Example | Shows |
|---|---|
| [`examples/middlegen`](examples/middlegen/) | Every directive on one interface, all four kinds of generated middleware, when deferred writes really execute, generation into another package. Runs without any infrastructure. |
| [`examples/uow`](examples/uow/) | A ledger service on database/sql, sqlx and pgx with the same generated middlewares; `RunWith` boundaries with the check in the write, a `//middlegen:in-tx` task, SERIALIZABLE retries under concurrency and the nesting rules, against PostgreSQL. |
| [`examples/telemetry`](examples/telemetry/) | `InitTelemetry`, the endpoint middlewares on a go-kit endpoint through `telemetry.Kit`, trace propagation over HTTP and gRPC metadata, the go-kit log adapter and the fan-out handler. |
| [`examples/connectrpc`](examples/connectrpc/) | An SDK layout on the typed endpoints: a `Service` interface implemented by the server and by the Connect client alike, endpoints, a Connect handler that can be backed by another server (a gateway), the generated decorators on both sides through `middleware.Chain` and `telemetry.Metrics` on the endpoints. |

```bash
cd examples
go run ./middlegen                      # no infrastructure needed
docker compose up -d && go run ./uow    # PostgreSQL and an OTel Collector
```

See [`examples/README.md`](examples/README.md) for the details.

---

## Releasing

Releases are cut from the GitHub Actions tab: run the **Release** workflow with the version to publish (for example `v0.1.0`). It verifies the module on the selected ref, creates the annotated tag, publishes a GitHub Release with generated notes, and asks the Go module proxy to index the new version.

---

## Directory Structure

```
silo/
├── cmd/
│   └── middlegen/       # Middleware decorator generator CLI
├── connectrpc/          # ConnectRPC adapters for type-safe endpoints
├── db/                  # SQL, sqlx and pgx database transaction adapters
├── docs/                # Design notes and review history
├── endpoint/            # Generic, type-safe endpoint signature
├── examples/            # Runnable examples (separate Go module)
├── middleware/          # Generic middleware type definitions
├── telemetry/           # OpenTelemetry trackers, exporters, and middlewares
└── uow/                 # Core Unit of Work transaction orchestrator
```

---

## Architecture Overview

```mermaid
graph TD
    Service[Service Layer] -->|Uses| UoW[uow.Manager]
    UoW -->|Wraps in Transaction| DB[db.SQLTransactor / db.SQLXTransactor / db.PGXTransactor]
    Service -->|Decorated by| Middleware[Generated Middlewares]
    Middleware -->|Publishes| Telemetry[telemetry.Metrics / Tracing]
```

---

## The transaction model

`uow.Manager` runs one model: the unit of work. A **boundary** (`RunWith`) runs business logic first and commits the writes it queued afterwards, in one transaction. A **task** is the transactional part: it runs with the transaction open, and repository calls made from a task execute immediately. `RunInTx` runs one function as a task.

### `RunWith`: the boundary

1. The business action runs **first, outside any transaction**, with a context carrying a fresh `uow.UnitOfWork`. Repository reads made here go straight to the connection pool.
2. Repository methods decorated with the `uow_repo` middleware do not execute; they **queue** themselves on the unit of work and return immediately (see [What deferred methods return](#what-deferred-methods-return)).
3. When the action returns `nil`, a transaction is opened and the queued tasks run inside it, in order, followed by a commit. No transaction is opened when nothing was queued.
4. If a task or the commit fails with an error the retry evaluator accepts, the **queued tasks** are re-run in a fresh transaction. The action is not re-run, so the closures execute with the values they captured in step 1.

The transaction, its isolation level and the retries therefore cover the queued tasks only. The action never holds a connection or a lock, which is what makes it safe to call other services, HTTP clients or queues from it.

### `RunInTx`: one task

1. The transaction is opened **first** and the function runs inside it. Its context carries the transaction but **no unit of work**, so reads run under the transaction's isolation level and decorated writes execute at once, with real results.
2. The transaction commits when the function returns `nil` and rolls back when it returns an error.
3. On a retryable error the **whole function** is re-run in a fresh transaction. A task therefore only talks to the database: an HTTP call made from it would hold the transaction open for the round trip and be repeated on retry.

Called inside a `RunWith` action, `RunInTx` opens nothing: it queues the function on that boundary's unit of work and returns `nil` at once. The function runs in the boundary's transaction, in `Defer` order, and its error is returned by the boundary. Called inside a task, the function runs immediately. See [Nesting](#nesting).

The generated `uow_service` middleware wraps every method in `RunWith`; a method marked `//middlegen:in-tx` runs as one task through `RunInTx` instead. Such a method must take a `context.Context` and return only an `error`, because a queued call returns before its body runs (see the [directives](#directives)).

Mark a method `in-tx` when all of the following hold; otherwise keep the boundary:

- It reads and then writes, and no single statement can carry the check. When one can, [put the check in the write](#read-modify-write-put-the-check-in-the-write) instead.
- It talks only to the database. The transaction stays open for the whole body and a retry runs the body again, so an HTTP call or a message published from it is held for the round trip and repeated.
- Its callers need nothing back but an error. A boundary that calls it gets `nil` at the call and the error from its own return (see the [flow comparison](#flow-comparison-a-service-calling-a-service)).
- It is short: it holds a connection and its locks from `BEGIN` to `COMMIT`.

### Read-modify-write: put the check in the write

Under `RunWith`, a check made in the action and a write queued on it are not linked. Take a `Transfer` that reads a balance, compares it with the amount, and queues a debit, with two concurrent transfers of 8000 from an account holding 10000: both read 10000 on the pool, both pass the check, both debits are queued, and either the balance goes to -6000 or a `CHECK` constraint rejects the second one with a hard error. Neither is a retry.

The unit of work's answer is to let the database check and write atomically, in one statement:

```sql
UPDATE accounts SET balance = balance + $1 WHERE id = $2 AND balance + $1 >= 0
```

The repository maps "no row updated" to `ErrInsufficientFunds`; the service queues the debit as before and gets that error back from the boundary. This needs no isolation level and no retry, the row lock lasts for one statement, and the service method stays a plain `RunWith` boundary that composes with any other. Keep a `CHECK (balance >= 0)` constraint as the safety net. The [uow example](examples/uow/) does exactly this.

When one statement cannot express the invariant (a rule over several rows or tables), do the read and the write in a **task**, where the transaction is open: a hand-written `Defer`, or a service method marked `//middlegen:in-tx`. Lock what you read (`SELECT ... FOR UPDATE`) or use a `SERIALIZABLE` transactor with `db.IsRetryableTxError` as the retry evaluator, so a conflicting task is re-run instead of committing on stale data.

Two things no boundary can do: branch in the action on the outcome of a queued write, since the write has not happened yet, and call external services from a task. A flow that needs either is a workflow, not a unit of work.

### Using the unit of work directly

The generated `uow_repo` middleware is a convenience: any code can queue work on the unit of work carried by the context. The executor helpers in `db` pick the transaction out of the context the task receives.

```go
err := manager.RunWith(ctx, func(ctx context.Context) error {
	unit, _ := uow.Extract(ctx) // present inside a RunWith action
	unit.Defer(func(txCtx context.Context) error {
		_, err := db.PGXExecutor(txCtx, pool).Exec(txCtx,
			"INSERT INTO users (id, name) VALUES ($1, $2)", id, name)
		return err
	})
	return nil // the transaction opens here and runs the queued task
})
```

`RunInTx` is the same task on its own: the transaction is open, so every statement runs where it is written, and the whole function is re-run on a retryable error.

```go
err := manager.RunInTx(ctx, func(txCtx context.Context) error {
	ex := db.PGXExecutor(txCtx, pool)
	var balance int64
	if err := ex.QueryRow(txCtx, "SELECT balance FROM accounts WHERE id = $1 FOR UPDATE", id).Scan(&balance); err != nil {
		return err
	}
	_, err := ex.Exec(txCtx, "UPDATE accounts SET balance = $1 WHERE id = $2", balance+interest(balance), id)
	return err
})
```

### Nesting

Boundaries nest by joining what the context already carries. The two cases:

| The context carries | `RunWith` | `RunInTx` |
|---|---|---|
| A unit of work (inside a `RunWith` action) | Joins: runs the action now; the work it defers belongs to the outer unit. | Queues the task on that unit and returns `nil` at once. The task runs in the boundary's transaction, in `Defer` order, and its error is returned by the boundary. |
| An open transaction and **no unit of work** (inside a task) | Runs the action in that transaction with a fresh unit whose tasks run right after it; the outer boundary commits. | Runs the task now. |

A second transaction is never opened inside a first one, and a `RunWith` method may call an `in-tx` method or the other way round: both compose, and the [flow comparison](#flow-comparison-a-service-calling-a-service) below draws each combination. `uow.InTransaction(ctx)` tells any code which situation it is in. A task that calls `Defer` on the unit that is executing it, or a goroutine that outlives the action and defers late, is reported with `uow.ErrLateDefer` instead of being silently dropped. Contexts handed to actions and tasks must not outlive their boundary.

### Flow comparison: a service calling a service

Every method of a `uow_service` interface is either a boundary (`RunWith`, the default) or a task (`//middlegen:in-tx`). When a method of service `A` calls a method of service `B`, those two kinds decide the shape of the call. The diagrams show the four combinations: `A` and `B` are generated `uow_service` middlewares, `repo` is a `uow_repo` middleware, and `manager` is the shared `uow.Manager`.

**1. `A` is a boundary and calls no other service.** The action runs first, on the pool, and one transaction then runs what it queued. A retryable error re-runs the part between `BEGIN` and `COMMIT`; the action does not run again.

```mermaid
sequenceDiagram
    participant C as caller
    participant A as A.Method (RunWith)
    participant M as manager
    participant R as repo
    participant DB
    C->>A: Method(ctx)
    A->>M: RunWith(action)
    M->>A: action(ctx + unit)
    A->>R: Get (non-transactional)
    R->>DB: SELECT on the pool
    A->>R: Save
    R-->>A: queued, returns the item
    A-->>M: nil
    rect rgba(127, 127, 127, 0.12)
    M->>DB: BEGIN
    M->>R: Save (queued task)
    R->>DB: INSERT
    M->>DB: COMMIT
    end
    M-->>C: nil, or the task's error
```

**2. `A` is a boundary and calls `B`, which is `in-tx`.** The call queues `B`'s body on `A`'s unit of work and returns `nil` at once. The body keeps its place in the queue and runs with the transaction open, between the writes `A` queued before and after the call. `A`'s action cannot see `B`'s outcome: a read made after the call goes to the pool and sees the old state, and `B`'s error is what `A`'s `RunWith` returns. If `A`'s action returns an error, nothing runs, `B`'s body included. The logging and tracing middlewares around `B` see only the queuing call: `B`'s span ends before its body runs, the body's repository calls are traced under `A`'s span, and a failure is logged as `A`'s.

```mermaid
sequenceDiagram
    participant C as caller
    participant A as A.Method (RunWith)
    participant B as B.Method (in-tx)
    participant M as manager
    participant R as repo
    participant DB
    C->>A: Method(ctx)
    A->>M: RunWith(action)
    M->>A: action(ctx + unit)
    A->>B: Method(ctx)
    B->>M: RunInTx(task)
    M-->>B: queued on A's unit
    B-->>A: nil, the body has not run
    A->>R: Save
    R-->>A: queued
    A-->>M: nil
    rect rgba(127, 127, 127, 0.12)
    M->>DB: BEGIN
    M->>B: task(txCtx), B's body runs now
    B->>R: Get, Save
    R->>DB: SELECT, UPDATE at once
    M->>R: Save (A's queued task)
    R->>DB: INSERT
    M->>DB: COMMIT
    end
    M-->>C: nil, or B's error, or the task's
```

**3. `A` is `in-tx` and calls `B`, which is a boundary.** The transaction is open before `A`'s body. `B`'s action runs at once inside it with a fresh unit of work; the writes `B` queues run as soon as its action returns, still inside `A`'s transaction, and the call returns `B`'s real result to `A`. Everything `B` does now happens inside a transaction, including whatever its action was written to do outside one, and a retry re-runs `A`'s whole body, `B` included.

```mermaid
sequenceDiagram
    participant C as caller
    participant A as A.Method (in-tx)
    participant B as B.Method (RunWith)
    participant M as manager
    participant R as repo
    participant DB
    C->>A: Method(ctx)
    A->>M: RunInTx(task)
    rect rgba(127, 127, 127, 0.12)
    M->>DB: BEGIN
    M->>A: task(txCtx)
    A->>R: Get, Save
    R->>DB: SELECT, UPDATE at once
    A->>B: Method(txCtx)
    B->>M: RunWith(action)
    M->>B: action(txCtx + fresh unit)
    B->>R: Save
    R-->>B: queued on B's unit
    B-->>M: nil
    M->>R: Save (B's queued task)
    R->>DB: INSERT
    M-->>B: nil, or the task's error
    B-->>A: B's result, now
    A-->>M: nil
    M->>DB: COMMIT
    end
    M-->>C: nil, or the error
```

**4. Both are `in-tx`.** `A` opens the transaction and `B`'s body runs immediately inside it, returning its real result. One transaction, one commit, and one retry that re-runs both.

```mermaid
sequenceDiagram
    participant C as caller
    participant A as A.Method (in-tx)
    participant B as B.Method (in-tx)
    participant M as manager
    participant R as repo
    participant DB
    C->>A: Method(ctx)
    A->>M: RunInTx(task)
    rect rgba(127, 127, 127, 0.12)
    M->>DB: BEGIN
    M->>A: task(txCtx)
    A->>B: Method(txCtx)
    B->>M: RunInTx(task)
    M->>B: task(txCtx), runs now
    B->>R: Get, Save
    R->>DB: SELECT, UPDATE at once
    B-->>A: B's result, now
    A-->>M: nil
    M->>DB: COMMIT
    end
    M-->>C: nil, or the error
```

Side by side:

| | 1. boundary alone | 2. boundary calls `in-tx` | 3. `in-tx` calls boundary | 4. `in-tx` calls `in-tx` |
|---|---|---|---|---|
| The transaction opens | after `A`'s action | after `A`'s action | before `A`'s body | before `A`'s body |
| `B`'s body runs | n/a | in `A`'s transaction, at its place in `A`'s queue | at once inside `A`'s transaction, then `B`'s queued writes | at once inside `A`'s transaction |
| The call returns to `A` | n/a | `nil`, before the body runs | `B`'s real result | `B`'s real result |
| `A` can branch on `B`'s outcome | n/a | no, `B`'s error is `A`'s return value | yes | yes |
| A retryable error re-runs | `A`'s queued tasks | `A`'s queued tasks, `B`'s body included | `A`'s whole body, `B` included | `A`'s whole body, `B` included |
| Where external calls are safe | `A`'s action | `A`'s action | nowhere, the transaction is open throughout | nowhere, the transaction is open throughout |

Two rules produce all four shapes: a call made with a unit of work in the context is queued on it, and a call made with a transaction and no unit runs now, inside it. Shape 2 is the one to watch. The `in-tx` method behaves like a queued write there, so give it only work whose result the caller does not need until the commit. When `A` must act on `B`'s outcome, make `A` `in-tx` as well (shape 4) or move the decision into a conditional write (see [Read-modify-write](#read-modify-write-put-the-check-in-the-write)).

### Multiple databases

The `db` package stores one active transaction per context, so nesting boundaries that belong to **different databases** is not supported: the inner tasks would run against the outer transaction. Its executor helpers refuse to hand out a pool while a transaction of the other driver family is active in the context (see [Choosing a Transactor](#choosing-a-transactor)).

### Retrying serialization failures

```go
transactor := db.NewPGXTransactor(pool, db.WithPGXTxOptions(pgx.TxOptions{IsoLevel: pgx.Serializable}))
manager := uow.NewManager(transactor,
    uow.WithRetryEvaluator(db.IsRetryableTxError), // SQLSTATE 40001 and 40P01
    uow.WithMaxRetries(3),
    uow.WithRetryDelay(50*time.Millisecond, time.Second),
)
```

`db.IsRetryableTxError` recognises PostgreSQL `serialization_failure` (40001) and `deadlock_detected` (40P01) through any driver whose errors expose `SQLState()`, which includes pgx and lib/pq. Retries back off exponentially from the base delay up to the cap, minus up to 25% random jitter so colliding workers do not retry in lock-step. Without `WithRetryEvaluator` nothing is retried; the budget defaults to 3 retries, backing off from 50ms up to 500ms.

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

//go:generate go tool middlegen -type=UserRepository -kinds=uow_repo,logging,tracing
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

The service layer orchestrates business logic and manages the Unit of Work lifecycle boundaries. We use the `uow_service` kind to auto-wrap service execution in `uow.Manager.RunWith` boundaries: writes queued by the repository are committed in one transaction when the service method returns, with automatic retries of transient failures. A method that must read and write inside the transaction (a rule over several rows) is marked `//middlegen:in-tx` and runs as one task through `RunInTx` instead; see [Read-modify-write](#read-modify-write-put-the-check-in-the-write).

`service/user_service.go`:
```go
package service

import (
	"context"

	mydb "my-app/db"
)

//go:generate go tool middlegen -type=UserService -kinds=uow_service,logging,tracing
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
The `//go:generate go tool middlegen` lines use the generator recorded in `go.mod` (see [Installation](#installation)); with an installed binary write `//go:generate middlegen ...` instead, and with neither use the `go run ...@v0.3.0` form. Regenerating is always safe: previously generated `.gen.go` files are ignored while the package is loaded, so stale output that no longer compiles does not block the generator.

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

	// Apply Repository decorators (ordering: innermost is raw implementation).
	// Build them after InitTelemetry: the logging middleware captures
	// slog.Default() when its constructor runs.
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
| `db.NewPGXTransactor(pool)` | `pgx.Tx` | Repositories use native `pgx` via `db.PGXExecutor`. `db.PGXCommon` covers `Exec`, `Query`, `QueryRow`, `SendBatch` and `CopyFrom`, so batches and COPY run inside the transaction too. |

Each family lives in its own file of the `db` package (`sql.go`, `sqlx.go`, `pgx.go`), and the database/sql and sqlx transactors share one context key so a plain `database/sql` repository works inside a transaction begun by `SQLXTransactor` and vice versa. The pgx transactor uses its own key. The executor helpers never hand out the pool while a transaction of the **other** family is active in the context: `db.Executor` and `db.XExecutor` panic when they find a pgx transaction, and `db.PGXExecutor` panics when it finds a database/sql one. That mismatch means the repository and the transactor were built for different drivers, and running the statement on the pool would silently put it outside the transaction. Code that intentionally targets another database from inside a unit of work uses its own pool directly instead of an executor helper.

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
| `-service` | *(Inferred)* | Telemetry name: the `service` log attribute, the tracer and meter name, and the span-name prefix. Defaults to the lowercased package name. |
| `-dir` | `""` | Directory, relative to the module root, of the package declaring the interface. The generated files are written to the working directory and belong to its package (see [Generating into another package](#generating-into-another-package)). |
| `-prefix` | `middlegen` | Directive prefix namespace for comment annotations (`//<prefix>:...`). |
| `-middleware-import` | *(Inferred)* | Import path of the generic `Middleware` helper package. |
| `-middleware-type` | `middleware.Middleware` | The type signature representation for middlewares. |
| `-library-module` | `github.com/pobochiigo/silo` | Module path providing the `middleware`/`telemetry`/`uow` packages referenced by generated code. |

### Directives

Directives are comments on the methods of the interface, written `//<prefix>:<directive> [arguments]` with no space after `//` (the default prefix is `middlegen`, see `-prefix`). The rules:

- A directive goes in the method's doc comment (the lines above it) or in its trailing line comment. Several directives can be stacked on one method, mixed with ordinary comment lines.
- Directives written on the methods of an embedded interface apply wherever that interface is embedded, whichever package declares it.
- Parameter lists are separated by commas or spaces and use the names from the interface declaration, even when the generator renames a parameter internally.
- A directive that names an unknown parameter, or an `echo` that lists more parameters than the method has non-error results, aborts generation with an error naming the method.
- Directives for kinds that are not being generated are ignored, so one interface can carry the directives of all five kinds.

| Directive | Applies to | Effect |
|---|---|---|
| `//middlegen:non-transactional` | `uow_repo` | Run the method immediately even inside a unit of work. Use it for reads. |
| `//middlegen:in-tx` | `uow_service` | Run the method as one task through `Manager.RunInTx`: the transaction is open before the body, decorated writes execute at once, and the whole body is re-run on a retryable error. Called inside a `RunWith` boundary the method is queued on it. The method must take a `context.Context` and return only an `error`; this is checked when `uow_service` is generated. |
| `//middlegen:echo <param>[, <param>...]` | `uow_repo` | Return the named parameters, in order, as the deferred method's non-error results. `echo none` disables echoing. |
| `//middlegen:redact <param>[, <param>...]` | `logging` | Log the named parameters as `[REDACTED]`. |
| `//middlegen:metric attr:<name>=<expr>` | `metrics` | Add a metric attribute computed from a Go expression over the parameters. |
| `//middlegen:metric counter:<name>` | `metrics` | Increment a custom counter on every call. |

> **Cardinality warning:** metric attributes become label values on every series. Never derive them from unbounded inputs such as user or order IDs.

### Directives by example

The interface below uses every directive. The excerpts that follow are what `middlegen -type=Repository -kinds=uow_repo,logging,tracing,metrics -service=accounts` generates for it, shortened to the relevant methods and annotated with `// <-` comments.

`account/repository.go`:
```go
package account

import "context"

type Account struct {
	ID      string
	Balance int64
}

// Reader is embedded by Repository; the directives on its methods carry over.
type Reader interface {
	//middlegen:non-transactional
	GetByID(ctx context.Context, id string) (*Account, error)

	List(ctx context.Context, limit int) ([]Account, error) //middlegen:non-transactional
}

//go:generate go tool middlegen -type=Repository -kinds=uow_repo,logging,tracing,metrics -service=accounts
type Repository interface {
	Reader

	// Exactly one parameter has the result's type, so a deferred Save hands
	// acc straight back to the caller.
	//middlegen:metric counter:account_saves_total
	Save(ctx context.Context, acc *Account) (*Account, error)

	// Two parameters match the result: say which one to return.
	//middlegen:echo dst
	Merge(ctx context.Context, src, dst *Account) (*Account, error)

	// Basic-typed results are never echoed; the secret never reaches the logs.
	//middlegen:redact secret
	RotateKey(ctx context.Context, id string, secret string) (string, error)

	// reason is a small, fixed set of values: safe as a metric attribute.
	//middlegen:metric attr:reason=reason
	Delete(ctx context.Context, id string, reason string) error
}
```

**`uow_repo`** (`repository_uow_middleware.gen.go`): reads run immediately; writes are queued on the unit of work found in the context and the caller gets its own object back. Without a unit of work in the context (outside any boundary, or inside a deferred task) every method is a plain pass-through.

```go
func (m *repositoryUoWMiddleware) GetByID(ctx context.Context, id string) (*Account, error) {
	return m.next.GetByID(ctx, id) // <- non-transactional: never deferred
}

func (m *repositoryUoWMiddleware) Save(ctx context.Context, acc *Account) (*Account, error) {
	if uowInstance, ok := uow.Extract(ctx); ok {
		uowInstance.Defer(func(txCtx context.Context) error {
			_, err := m.next.Save(txCtx, acc)
			return err
		})
		return acc, nil // <- echoed: the only *Account parameter
	}
	return m.next.Save(ctx, acc)
}

func (m *repositoryUoWMiddleware) Merge(ctx context.Context, src *Account, dst *Account) (*Account, error) {
	if uowInstance, ok := uow.Extract(ctx); ok {
		uowInstance.Defer(func(txCtx context.Context) error {
			_, err := m.next.Merge(txCtx, src, dst)
			return err
		})
		return dst, nil // <- //middlegen:echo dst
	}
	return m.next.Merge(ctx, src, dst)
}

func (m *repositoryUoWMiddleware) RotateKey(ctx context.Context, id string, secret string) (string, error) {
	if uowInstance, ok := uow.Extract(ctx); ok {
		uowInstance.Defer(func(txCtx context.Context) error {
			_, err := m.next.RotateKey(txCtx, id, secret)
			return err
		})
		return "", nil // <- basic result: zero value while deferred
	}
	return m.next.RotateKey(ctx, id, secret)
}

func (m *repositoryUoWMiddleware) Delete(ctx context.Context, id string, reason string) error {
	if uowInstance, ok := uow.Extract(ctx); ok {
		uowInstance.Defer(func(txCtx context.Context) error {
			return m.next.Delete(txCtx, id, reason)
		})
		return nil // <- error-only result: the write is queued, nothing to echo
	}
	return m.next.Delete(ctx, id, reason)
}
```

**`logging`** (`repository_logging_middleware.gen.go`): one `Debug` line per call with every parameter, one `Error` line per failure. Redacted parameters keep their key.

```go
func RepositoryLoggingMiddleware() middleware.Middleware[Repository] {
	logger := slog.Default().With(slog.String("service", "accounts")) // <- captured now, not per call
	return func(next Repository) Repository {
		return &repositoryLoggingService{Repository: next, next: next, logger: logger}
	}
}

func (l *repositoryLoggingService) RotateKey(ctx context.Context, id string, secret string) (string, error) {
	l.logger.DebugContext(ctx, "RotateKey started", slog.Any("id", id), slog.String("secret", "[REDACTED]"))
	r0, err := l.next.RotateKey(ctx, id, secret)
	if err != nil {
		l.logger.ErrorContext(ctx, "RotateKey failed", slog.Any("error", err))
	}
	return r0, err
}
```

**`metrics`** (`repository_metrics_middleware.gen.go`): the standard request, error and latency instruments are named after the interface; custom counters and attributes come from the directives.

```go
func RepositoryMetricsMiddleware() middleware.Middleware[Repository] {
	meter := otel.GetMeterProvider().Meter("accounts")
	recorder := telemetry.NewMetricsRecorder(meter, "repository") // <- repository_requests_total, repository_errors_total, repository_request_duration_seconds
	accountSavesTotalCounter, err := recorder.Meter().Int64Counter("account_saves_total", metric.WithDescription("Custom counter for account_saves_total"))
	if err != nil {
		otel.Handle(err)
	}
	// ...
}

func (m *repositoryMetricsService) Save(ctx context.Context, acc *Account) (*Account, error) {
	now := time.Now()
	m.accountSavesTotalCounter.Add(ctx, 1) // <- //middlegen:metric counter:account_saves_total
	r0, err := m.next.Save(ctx, acc)
	m.recorder.Observe(ctx, "Save", now, err) // <- method="Save" on every instrument
	return r0, err
}

func (m *repositoryMetricsService) Delete(ctx context.Context, id string, reason string) error {
	now := time.Now()
	err := m.next.Delete(ctx, id, reason)
	m.recorder.Observe(ctx, "Delete", now, err, attribute.String("reason", fmt.Sprintf("%v", reason))) // <- //middlegen:metric attr:reason=reason
	return err
}
```

**`tracing`** (`repository_tracing_middleware.gen.go`): one internal span per call, named `<service>.<Method>`, with the error recorded and the span status set on failure.

```go
func (t *repositoryTracingService) Save(ctx context.Context, acc *Account) (*Account, error) {
	ctx, span := t.tracer.Start(ctx, "accounts.Save", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	r0, err := t.next.Save(ctx, acc)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return r0, err
}
```

### What each kind generates

Every kind produces one file, `<iface><suffix>`, where `<iface>` is the interface name in snake case (`UserRepository` becomes `user_repository`). Each file holds a constructor that returns a `middleware.Middleware[T]` (a `func(T) T`), so decorators chain by application: the innermost call wraps the raw implementation.

| Kind | Constructor | File suffix | Behaviour |
|---|---|---|---|
| `logging` | `<Iface>LoggingMiddleware()` | `_logging_middleware.gen.go` | `slog.Default()` (captured when the constructor runs) with `service=<service>`. `<Method> started` at `Debug` with every parameter, `<Method> failed` at `Error` with `error`. Methods with a context use the `*Context` variants so records carry the trace and span IDs. |
| `tracing` | `<Iface>TracingMiddleware()` | `_tracing_middleware.gen.go` | Span `<service>.<Method>` of kind internal from `otel.Tracer(<service>)`; on error `RecordError` and status `Error`. Methods without a context are forwarded unchanged. |
| `metrics` | `<Iface>MetricsMiddleware()` | `_metrics_middleware.gen.go` | On meter `<service>`: `<iface>_requests_total`, `<iface>_errors_total` and `<iface>_request_duration_seconds` (seconds, `DefaultLatencyBuckets`) with attribute `method` plus the `metric attr` attributes; one `Int64Counter` per `metric counter` name, without attributes. Methods without a context are measured with a background context. |
| `uow_repo` | `<Iface>UoWMiddleware()` | `_uow_middleware.gen.go` | Methods with a context and without `non-transactional` are queued on the unit of work in the context and return immediately (see [What deferred methods return](#what-deferred-methods-return)); everything else passes through. |
| `uow_service` | `<Iface>UoWMiddleware(manager *uow.Manager)` | `_uow_middleware.gen.go` | Every method with a context runs inside `manager.RunWith`, so the writes it makes through decorated repositories commit when it returns; a method marked `//middlegen:in-tx` runs as one task through `manager.RunInTx`. A nested call joins the outer boundary (see [Nesting](#nesting) and the [flow comparison](#flow-comparison-a-service-calling-a-service)). |

`uow_repo` and `uow_service` write the same file and constructor name, so generate one or the other for a given interface: repositories get `uow_repo`, the services calling them get `uow_service`.

### Generating into another package

With `-dir`, the interface can live in a different package than the generated decorators. Run the generator from the destination package; `-dir` is the interface's directory relative to the module root:

`decorators/doc.go`:
```go
// Package decorators holds the middlewares generated for interfaces declared elsewhere.
package decorators

//go:generate go tool middlegen -type=UserRepository -dir=db -kinds=logging,metrics -service=users
```

The generated file belongs to `package decorators`, imports the interface's package, and refers to it qualified:

```go
func UserRepositoryLoggingMiddleware() middleware.Middleware[db.UserRepository] {
```

The destination package must not be imported by the interface's package, or the generated import creates a cycle.

### What deferred methods return

A `uow_repo` method that runs inside a unit of work is queued, not executed, so it has to return *something* immediately. Silo hands the caller back the object it passed in, which is the source of truth for a write that has not happened yet:

- `//middlegen:echo` decides explicitly which parameters are returned.
- Otherwise, a result is echoed when **exactly one** parameter has its type (a `*T` parameter also satisfies a `T` result, guarded against `nil`, and vice versa) and that type is not a basic type (`string`, `int`, `bool`, ...). `Save(ctx, user *User) (*User, error)` returns `user`.
- Every other result is its zero value. Basic-typed results are never echoed from parameters; when several parameters are candidates, the generator warns and returns the zero value until you add an `echo` directive.

Read methods must be annotated `//middlegen:non-transactional` so they execute immediately and return real data. Inside a task, whether deferred by hand or a `//middlegen:in-tx` method, the context carries no unit of work, so a decorated call made there executes immediately as well and returns real results.

### Notes on generated code

- `middlegen` type-checks the package with `go/packages`, so it needs the `go` tool and resolvable module dependencies. Type errors caused by not-yet-generated code are reported and tolerated; errors in the interface's own signatures abort generation.
- Methods of **embedded interfaces** are decorated like any other, whether the embedded interface lives in the same package, another package of your module, a dependency, or the standard library (`io.Closer`), and however deeply the embeddings nest. Directives written on the embedded interface's methods apply wherever it is embedded. The one exception is an unexported method declared in another package: Go does not allow implementing it from outside, so it is forwarded through the embedded field and a warning names it.
- Parameters whose names collide with identifiers used by the templates (`t`, `m`, `err`, `ok`, `span`, the packages the templates import such as `time` or `uow`, and the qualifiers of packages your signatures use) are transparently renamed in the generated code; log attribute keys and `metric attr` expressions keep the original names. Blank (`_`) and unnamed parameters become `p0`, `p1`, .... The context parameter may appear at any position.
- Types from other packages are qualified from type information, so a package imported under an alias in your file, two packages sharing a name, or a package whose name differs from its path's last element (`pgx/v5`) are all imported correctly in the generated files.
- The logging middleware logs the `<Method> started` line with all parameters at `Debug` level and failures at `Error` level. Redact secrets with `//middlegen:redact`.
- The logging middleware captures `slog.Default()` when its constructor is called, so call `<Iface>LoggingMiddleware()` after `telemetry.InitTelemetry` (or your own `slog.SetDefault`), otherwise its records bypass the OTLP pipeline. Tracers and meters are taken from the global OpenTelemetry providers, which delegate to whatever provider is registered later, so their construction order does not matter.
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

The package depends on the standard library only, so an SDK built on it (a set of `Service` interfaces implemented by Connect clients and servers alike) pulls nothing else into its consumers' module graphs. The [endpoint middlewares](#endpoint-middlewares) of the `telemetry` package decorate a typed endpoint directly, and `middleware.Chain` composes middlewares of one type, the generated decorators included:
```go
eps.Greet = middleware.Chain(
	telemetry.Tracing[*GreetRequest, *GreetResponse]("greeter.Greet"),
	telemetry.Metrics[*GreetRequest, *GreetResponse](recorder, "Greet"),
)(eps.Greet)
```
The type arguments are spelled out because Go cannot infer them from a string; generated SDK code does not mind. A go-kit middleware, such as go-kit's rate limiter or circuit breaker, is applied to a typed endpoint through `telemetry.Adapt[Req, Resp](mw)`; one that replaces the request or the response with a value of another type makes the adapted endpoint return an error rather than panic.

### ConnectRPC Adapters
The `connectrpc` package adapts these type-safe endpoints to ConnectRPC server handlers and client endpoints. The endpoint sees only the decoded message: request headers, response headers and trailers are not exposed. Handle them in a Connect interceptor, or read them in the decoder, which receives the raw `*connect.Request`'s message and context.

The [connectrpc example](examples/connectrpc/) shows the layout these adapters are meant for: a feature package with the domain types, a `Service` interface, its `Endpoints` and a `NewXHandler(svc)` constructor, and a client package whose `NewXClient(httpClient, baseURL, opts...)` returns the same `Service`. Because the client is a `Service`, a handler can be backed by a client of another server, and decorators written for the service, the generated ones included, fit both sides.

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
| `Endpoint` | The OTLP gRPC collector (Grafana Alloy, OTel Collector, ...) as `host:port` or a URL. A URL's scheme decides transport security (`http://` plaintext, `https://` TLS). In both forms a missing port defaults to 4317, not to gRPC's 443. When set, the three exporters share **one** gRPC connection. When empty, each exporter dials on its own following `OTEL_EXPORTER_OTLP_ENDPOINT` and then the OTLP default `localhost:4317`. |
| `Insecure` | Disables TLS on exporter connections (plaintext gRPC). Defaults to `false` — TLS with the system certificate pool. Ignored when `Endpoint` is a URL. |
| `TLSConfig` | TLS settings for the exporter connections: a private CA in `RootCAs`, client certificates for mutual TLS. Nil uses the system pool. |
| `Compression` | `"gzip"` to compress exports; empty for none. |
| `DialOptions` | Extra gRPC dial options (keepalive parameters, resolvers) for the shared connection. |
| `Headers` | Extra gRPC metadata sent with every export, e.g. collector auth tokens. |
| `TraceSampleRatio` | Fraction of new traces to record, in `(0, 1]`; child spans follow their parent. Zero keeps the SDK default: `OTEL_TRACES_SAMPLER` if set, otherwise every trace. |
| `MaxExportBatchSize`, `BatchTimeout` | Batching of spans and log records. Defaults: 500 items, 5 seconds. |
| `MetricInterval` | How often metrics are pushed. Default: 30 seconds. |
| `SkipHostDetection` | Leaves `host.name` out of the resource. |
| `SkipSlogDefault` | Prevents `InitLogs` from replacing the process-wide `slog` default logger. The OTel logger provider is still registered globally, so `otelslog.NewHandler(name)` builds a handler on the exporter. |
| `LocalLogHandler` | Handler that receives every record in addition to the OTLP exporter. Defaults to a text handler on stderr. |
| `LocalLogLevel` | Minimum level of the default local handler. Defaults to Info; set `slog.LevelDebug` to see the generated middlewares' "started" lines locally. |
| `DisableLocalLogs` | Send logs to the collector only. Note that `slog.SetDefault` also routes the standard `log` package through slog, so nothing is written locally. |

The resource also carries the `telemetry.sdk.*` attributes and `host.name`. Its schema URL is the one the SDK's own resource detectors report, so it always matches the semantic conventions of the installed SDK; Silo does not pin one. Invalid configuration (a sample ratio outside `[0, 1]`, an unknown compressor, a malformed endpoint) is rejected by `InitTelemetry` before any exporter is created.

### Metrics

Every subsystem names its own series: `<subsystem>_requests_total`, `<subsystem>_errors_total` and `<subsystem>_request_duration_seconds`, with the attribute `method`. The subsystem is the interface name for a generated metrics middleware and the recorder's for `telemetry.Metrics`, so `NewMetricsRecorder(meter, "auth_endpoint")` yields `auth_endpoint_requests_total`. Latency histograms created by `NewMetricsRecorder` use `telemetry.DefaultLatencyBuckets`, second-scale boundaries from 5ms to 10s matching the Prometheus client defaults. The OpenTelemetry SDK's own defaults are sized for milliseconds and would put every request faster than five seconds into one bucket, making percentiles meaningless. Counters carry the unit `{request}` and histograms `s`.

`InitTraces` registers the W3C `TraceContext`/`Baggage` propagators globally. Applications that skip tracing but still forward trace headers can call `telemetry.InitPropagators()` directly.

### Logs

`InitLogs` installs a default `slog` logger that fans every record out to a local handler (stderr by default, or `LocalLogHandler`) **and** to the OTLP exporter through the official OTel bridge, so log lines carry the active trace and span IDs without disappearing from the machine when the collector is unreachable. `telemetry.NewFanoutHandler` is exported for building your own combinations, and the OTel logger provider is always registered globally, so a custom handler is one call away:

```go
import "go.opentelemetry.io/contrib/bridges/otelslog"

handler := telemetry.NewFanoutHandler(myLocalHandler, otelslog.NewHandler("my-app"))
slog.SetDefault(slog.New(handler))
```

Errors are logged under the `error` attribute everywhere in Silo: the endpoint middlewares, the go-kit log adapter and the generated middlewares.

### Endpoint middlewares
Typed middlewares for `endpoint.Endpoint[Req, Resp]`, each a `middleware.Middleware` so they compose with `middleware.Chain`:
- `Tracing[Req, Resp](operationName)`: an internal span named `operationName` around every call, with the error recorded and the status set on failure.
- `Logging[Req, Resp](operationName, logger)`: one `Info` record per success and one `Error` record per failure, with the operation, the duration and the error, through the context-aware slog methods so they carry the trace and span IDs. A nil logger resolves `slog.Default()` on every call.
- `Metrics[Req, Resp](recorder, operationName)`: one request, its latency and, on failure, one error on a `MetricsRecorder`, under `method=<operationName>`. The recorder's subsystem names the series (`auth_endpoint_requests_total` for `NewMetricsRecorder(meter, "auth_endpoint")`); share one recorder across the endpoints of a subsystem.

None of them looks at the request or the response, so the type parameters only carry the endpoint's types through. go-kit's `endpoint.Endpoint` is `endpoint.Endpoint[any, any]` under another name, so for a go-kit endpoint instantiate with `[any, any]` and convert with `telemetry.Kit`, a type conversion with no assertion:
```go
greet = telemetry.Kit(telemetry.Tracing[any, any]("greet"))(greet)
greet = telemetry.Kit(telemetry.Metrics[any, any](recorder, "greet"))(greet)
```
`MetricsMiddleware`, `LoggingMiddleware` and `TracingMiddleware` remain as deprecated go-kit wrappers over these. `MetricsMiddleware` now records on the subsystem `endpoint` (`endpoint_requests_total` with the attribute `method`) where it used to record `gokit_requests_total` with the attributes `operation` and `success`.

### Go-Kit Log Compatibility (`SlogAdapter`)
Bridges the gap between legacy `go-kit/log.Logger` interfaces and modern standard `log/slog`. The go-kit `level` value becomes the slog level, `msg` the message, `err`/`error` the `error` attribute, and the conventional `ts` timestamp is dropped because slog stamps its own. Route go-kit logs directly through your globally registered OTel bridge:
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
