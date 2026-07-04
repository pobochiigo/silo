# Silo: Standalone Unit of Work, Telemetry, and Middleware Generator

Silo is a high-performance, zero-dependency core library containing a transactional Unit of Work engine, OpenTelemetry integrations, and a middleware decorator generator for standard Go interfaces. 

## Features

- **Unit of Work (UoW)**: A driver-agnostic transaction coordinator that queues tasks to execute in a single database transaction. Out-of-the-box support for both Go standard library `database/sql` (`sqlx`) and native `pgx` (`pgxpool.Pool` and `pgx.Tx`).
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
├── db/                  # SQL and PGX database transaction adapters
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
	"errors"

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

	Save(ctx context.Context, user *User) error
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

func (r *postgresUserRepository) Save(ctx context.Context, user *User) error {
	executor := db.PGXExecutor(ctx, r.pool)

	_, err := executor.Exec(ctx, "INSERT INTO users (id, name) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET name=$2", user.ID, user.Name)
	return err
}
```

### Step 2: Define the Service & Annotate for `middlegen`

The service layer orchestrates business logic and manages the Unit of Work lifecycle boundaries. We use the `uow_service` kind to auto-wrap service execution in `uow.Manager.RunWith` boundaries, enabling transactional isolation and automated retries.

`service/user_service.go`:
```go
package service

import (
	"context"

	"github.com/pobochiigo/silo/db"
)

//go:generate middlegen -type=UserService -kinds=uow_service,logging,tracing
type UserService interface {
	CreateUser(ctx context.Context, id string, name string) error
}

type userService struct {
	repo db.UserRepository
}

func NewUserService(repo db.UserRepository) UserService {
	return &userService{repo: repo}
}

func (s *userService) CreateUser(ctx context.Context, id string, name string) error {
	user := &db.User{ID: id, Name: name}
	
	// When using uow_repo middleware, calling Save will queue the database operation.
	// It only executes and commits when this service block completes successfully.
	return s.repo.Save(ctx, user)
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

	// 1. Bootstrap Telemetry
	shutdown, err := telemetry.Bootstrap(ctx, telemetry.Config{
		ServiceName:    "user-service",
		ServiceVersion: "1.0.0",
		Endpoint:       "localhost:4317",
		Insecure:       true,
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
	uowManager := uow.NewManager(transactor)

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

## 4. `middlegen` CLI Reference

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
