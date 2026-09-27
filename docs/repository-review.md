# Repository review: `github.com/pobochiigo/silo`

Reviewed at commit `ac771b3` (2026-09-27). Scope: every non-test Go file in the
module, the tests, the README, and the CI workflow. All findings marked
**verified** were reproduced by generating code with `middlegen` and compiling
or running it, or by reading the relevant dependency source in the module
cache.

## Status

Every finding below has been addressed on this branch. The maintainer's
design note was kept: a deferred repository method hands the caller back the
object it passed in, because that object is the source of truth for a write
that has not happened yet. What changed per finding:

| Finding | Resolution |
|---|---|
| H1 | `ok` and every package the templates reference are reserved parameter names; a behavioural test wraps a spy and asserts the forwarded value. |
| H2 | Echo is now explicit or unambiguous: `//middlegen:echo <param>` (or `none`), otherwise the single parameter of a non-basic matching type, otherwise the zero value. Pointer dereferences are nil-guarded. Documented in the README. |
| H3 | README section "The transaction model" states what `RunWith` covers. `Manager.RunInTx` was added for read-modify-write flows: it opens the transaction first and re-runs the whole action on retry. |
| H4 | `uow_service` wrappers for error-less methods log the failed unit of work through `slog.Default()`; the generator warns per method. |
| M1 | Covered by the reserved list; `time` imports used in signatures are no longer dropped. |
| M2 | Imports are selected from the qualifiers actually used in signatures (AST selectors), with goimports-style assumed names (`yaml.v3` → `yaml`, `go-sqlmock` → `sqlmock`, `pgx/v5` → `pgx`). |
| M3 | `WithEndpoint` is only applied when a value is set; a test proves `OTEL_EXPORTER_OTLP_ENDPOINT` is honoured. |
| M4 | `InitLogs` installs a fan-out handler (stderr or `Config.LocalLogHandler`, plus OTLP); `Config.DisableLocalLogs` restores collector-only output. `NewFanoutHandler` is exported. |
| M5 | `//middlegen:redact <param>` logs `[REDACTED]`; the "started" line moved to `Debug`. |
| M6 | The dynamic `sqlx.Tx` wrapper always carries a mapper; an executor that cannot be adapted panics instead of escaping the transaction. |
| L1 | Metric attribute expressions are rewritten to the renamed parameters; README carries a cardinality warning. |
| L2 | Resource adds the SDK and environment detectors and the schema URL (semconv v1.41.0); the environment is emitted under both `deployment.environment.name` and `deployment.environment`. |
| L3 | Backoff subtracts up to 25% jitter and cannot overflow. |
| L4 | Stale skip entries removed. |
| L5 | Duplicate branch removed. |
| L6 | `toSnakeCase` keeps acronyms together (`GetByID` → `get_by_id`). |
| L7 | `NewPGXTransactor` panics at construction when options cannot be honoured. |
| L8 | Generic interfaces are rejected with a clear message. |
| L9 | Single-database assumption documented in the `uow` and `db` package docs and the README. |
| L10 | Header/trailer limitation documented in the README. |
| Tests | `SQLXTransactor`, every `XExecutor` branch, all transactor options, `IsRetryableTxError`, `RunInTx`, task-raised retries, backoff bounds, the fan-out handler, `NewResource`; golden files per template; a scaffolded module whose tests call the generated wrappers. |
| Docs | README walkthrough compiles as written; directory tree and numbering fixed; new directives, log behaviour and telemetry configuration documented. |
| CI | `go mod tidy` diff check, staticcheck, govulncheck, Dependabot for Go modules and Actions. |
| Follow-up | `middlegen` now loads the package with `go/packages` and works from type information: embedded interfaces (same package, other packages, standard library, nested) are decorated with their directives; foreign types are qualified and imported from `go/types` instead of string matching; parameters named `_`, contexts in any position, and package-name collisions are handled; interfaces without context methods, without context-and-error methods, or without deferrable methods no longer produce unused imports; methods without a context are still logged and measured. Templates live in `cmd/middlegen/templates/*.go.tmpl` and the generator is split into `generator.go`, `method.go`, `imports.go`, `directives.go`, `names.go` and `templates.go`. |

## Verdict

The library is small, cleanly layered, and the baseline is healthy: `gofmt`,
`go vet`, `go build` and `go test -race ./...` all pass locally. The Unit of
Work manager, the transactor adapters and the telemetry bootstrap are readable
and mostly correct.

Two areas need attention before this is safe to call production-ready:

1. **`middlegen` emits incorrect code for realistic interfaces.** A parameter
   named `ok` is silently replaced by `true` in deferred calls, a value result
   matched from a pointer parameter panics on `nil`, unrelated parameters are
   returned as results, and several ordinary parameter names (`time`, `uow`,
   `trace`, ...) or import combinations (`io` plus `bufio`) produce code that
   does not compile. The existing compile test does not cover these shapes.
2. **The Unit of Work guarantees are narrower than the README implies.** The
   business action, including every read, runs *before* a transaction is
   opened. Only the deferred writes are transactional, so isolation levels and
   serialization retries do not protect read-modify-write flows, and retried
   closures re-run with values captured outside the transaction.

Everything else is medium or low severity: a telemetry misconfiguration that
silently breaks export, an all-or-nothing log redirection, parameter logging
with no redaction, and a handful of nits.

## What is good

- Clear package boundaries: `uow` has no database dependency, `db` adapts three
  drivers to one small `Transactor` contract, `middleware` and `endpoint` are
  one-liners that keep generics at the edges.
- `uow.Manager.executeTransaction` handles the hard cases correctly: rollback
  on task error, rollback on panic with re-panic, rollback with a
  non-cancellable context, context checks between tasks and between retries.
- `db.PGXTransactor` fails loudly when `WithPGXTxOptions` is used with a pool
  that cannot honour it, instead of silently ignoring the options.
- `connectrpc.NewConnectServer` passes endpoint errors through untouched and
  only assigns Connect codes to decode/encode failures. That is the right
  split.
- `telemetry.InitTelemetry` unwinds already-started pipelines when a later one
  fails, and the composite shutdown returns the first error rather than the
  last.
- Global OTel accessors (`otel.Tracer`, `otel.Meter`,
  `otel.GetTextMapPropagator`) are captured at construction time in several
  places. That is safe: the global package delegates to a later-registered
  provider, so construction order relative to `InitTelemetry` does not matter.
- CI runs the race detector and pins `permissions: contents: read`.

## High severity

### H1. `uow_repo`: a parameter named `ok` is replaced by `true` in the deferred call (verified)

`cmd/middlegen/main.go:989-990` emits:

```go
if uowInstance, ok := uow.Extract(ctx); ok {
    uowInstance.Defer(func(txCtx context.Context) error {
        return m.next.SetFlag(txCtx, ok)   // this `ok` is the bool from Extract
    })
```

`ok` is not in `reservedParamNames` (`main.go:84-89`), so a method
`SetFlag(ctx context.Context, ok bool) error` compiles and silently forwards
`true` regardless of what the caller passed. A probe test confirmed: caller
passes `false`, the wrapped implementation receives `true`. If the parameter is
any other type the file fails to compile instead.

**Fix:** add `ok` to the reserved list, or rename the generated locals to
something no user would pick (for example `_uowInst, _uowOK`).

### H2. `uow_repo`: the "matching parameter" result heuristic returns wrong values and can panic (verified)

`main.go:449-478` builds the immediate return for deferred methods by looking
for the first non-context parameter whose type matches each non-error result,
including pointer/value variants. Three consequences:

- `Upsert(ctx, u *User) (User, error)` generates `return *u, nil`. A `nil`
  argument crashes inside the middleware with a nil-pointer dereference before
  the real implementation ever sees it. Reproduced.
- `Transfer(ctx, from, to string) (txID string, err error)` generates
  `return from, nil`. The caller receives the *source account* as the
  transaction ID. Reproduced.
- `Fetch(ctx, id string) (string, int, error)` returns `id` as the fetched
  value.

The README (`README.md:288`) says deferred methods return **zero values**, which
is what a reader would expect. The parameter-echo behaviour is undocumented and
unpredictable.

**Fix:** return genuine zero values (`getZeroValue` already handles every type
via `*new(T)`), or make the echo opt-in through a directive such as
`//middlegen:echo-arg user`. If the echo stays, never dereference a pointer.

### H3. Reads run outside the transaction; isolation and retry claims are overstated

`uow/uow.go:122-141`: `RunWith` executes the action first, collects the
deferred tasks, and only then calls `BeginTx`. In the README flow
(`README.md:56-149`) `GetByID` is `non-transactional` and hits the pool
directly, while `Save` is queued. So:

- A read-modify-write in a service method is not isolated at any isolation
  level, because the read is not inside the transaction.
- On a serialization-failure retry (`uow.go:143-167`), the same closures re-run
  with values captured during the pre-transaction action. The retry cannot
  observe fresh state.
- `README.md:117` ("enabling transactional isolation and automated retries")
  and `README.md:265` ("automatic retry of serialization failures") read as
  full guarantees.

**Fix:** document the model plainly (the transaction covers deferred writes
only; reads are dirty), and consider a second entry point, for example
`Manager.RunInTx`, that begins the transaction first, injects both the tx and
the UoW into the action's context, and retries the *whole action* on
retryable errors. That is the shape most callers expect from "Unit of Work".

### H4. `uow_service`: commit failures are discarded for methods without an error result

`main.go:1042-1057` generates `_ = m.manager.RunWith(ctx, ...)` for any
context-taking method that returns no `error`. If the deferred writes fail or
the commit fails, nothing surfaces. The caller believes the work happened.

**Fix:** refuse to generate `uow_service` wrappers for such methods (a clear
`log.Fatalf` with the method name), or at minimum log the error through
`slog.Default().ErrorContext`. Silent data loss is the worst outcome available.

## Medium severity

### M1. Generated bodies break when a parameter shadows a package used by the template (verified)

`Schedule(ctx context.Context, time time.Time) error` is legal Go, but every
generated file references the `time`, `uow`, `slog`, `trace`, `codes`, `fmt`,
`attribute`, `metric`, `telemetry` or `context` packages inside the method
body. Reproduced: `now := time.Now()` fails with `time.Now undefined (type
"time".Time has no field or method Now)` and the `uow_repo` file fails with
`undefined: time`.

**Fix:** add these package identifiers to `reservedParamNames`, or alias the
imports in the templates to names that cannot collide (for example `_slog`).

### M2. Import pruning uses substring matching (verified)

`main.go:522-543` keeps an import when `name + "."` appears in any parameter
or result type string. With `io` and `bufio` both imported and only
`*bufio.Reader` used, `io` is kept because `"io."` is a substring of
`"bufio."`, and the file fails with `"io" imported and not used`.
`format.Source` (`main.go:606`) is `gofmt`, not `goimports`, so it cannot
repair this. The inverse also fails: `gopkg.in/yaml.v3` resolves to the name
`yaml.v3` in `getImportName`, never matches `yaml.`, and the import is dropped.

**Fix:** run `golang.org/x/tools/imports.Process` on the output instead of
`format.Source`, or resolve package names from the parsed AST selectors rather
than string matching.

### M3. An empty `Endpoint` silently disables the OTLP default and env configuration (verified)

`telemetry/telemetry.go:97,128,161` always pass
`WithEndpoint(cfg.endpoint())`. In the exporter's `NewGRPCConfig`, the default
`localhost:4317` and `OTEL_EXPORTER_OTLP_ENDPOINT` are applied first and then
overwritten by options, so an empty string wins. `InitTelemetry` returns no
error; exports fail later and only reach `otel.Handle`.

**Fix:** append `WithEndpoint` only when the value is non-empty, or validate in
`InitTelemetry` and return an error. Either is a two-line change.

### M4. `InitLogs` routes *all* process logging to OTLP only

`telemetry.go:182-185` calls `slog.SetDefault(otelLogger)`. Two effects worth
documenting or changing:

- The OTel bridge writes only to the exporter. When the collector is down,
  every `slog` line is lost; nothing reaches stderr.
- `slog.SetDefault` also rewires the standard `log` package (verified in
  `log/slog/logger.go:62-74`), so `log.Printf` output disappears from local
  output too.

`SkipSlogDefault` is an escape hatch, but the default is the surprising
behaviour.

**Fix:** install a fan-out handler (stderr text/JSON plus the OTel handler) or
document loudly that local output stops. `slog-multi` style handlers are a
few dozen lines to write in-tree.

### M5. Logging middleware logs every parameter at Info with no redaction

`main.go:644` with `main.go:397` emits `slog.Any("<param>", <value>)` for every
non-context argument on every call. Passwords, tokens, and personal data in
request structs go straight to the log pipeline.

**Fix:** a `//middlegen:redact <param>` (or `//middlegen:no-log`) directive,
and consider `Debug` level for the "started" line so production logs are not
doubled per call.

### M6. `db.XExecutor` fallback branches are unsafe

`db/uow_db.go:62-85`:

- When the context holds a `*sql.Tx` but `fallback` is not a `*sqlx.DB`, the
  function returns `&sqlx.Tx{Tx: stdTx}` with a `nil` `Mapper`. sqlx's
  `Row.scanAny` calls `r.Mapper.TraversalsByName`, and `reflectx.Mapper.TypeMap`
  locks `m.mutex` on the nil receiver (verified in `sqlx.go:740-781`,
  `reflectx/reflect.go:104`). Any `GetContext`/`SelectContext` into a struct
  panics.
- When the context holds an executor that is neither `*sql.Tx` nor
  `SQLXCommon`, the function returns the pool. Writes then bypass the
  transaction and persist even if the Unit of Work rolls back.

**Fix:** set `Mapper: reflectx.NewMapperFunc("db", sqlx.NameMapper)` in the
first branch, and panic or return an error-carrying executor in the second.
Silent escape from a transaction is worse than a crash.

## Low severity

- **L1. Metric attribute directives reference pre-rename parameter names.**
  `//middlegen:metric attr:user_id = t` breaks once `t` is renamed to `tArg`
  (`main.go:760`). Also worth a README warning that attributes such as
  `user_id` explode metric cardinality.
- **L2. Resource omits SDK and environment detectors.** `telemetry.go:73-81`
  builds the resource from three attributes only. `resource.New` adds no
  defaults (verified), so `telemetry.sdk.*` attributes, `OTEL_SERVICE_NAME`
  and `OTEL_RESOURCE_ATTRIBUTES` are ignored and no schema URL is set. Add
  `resource.WithTelemetrySDK()`, `resource.WithFromEnv()` and
  `resource.WithSchemaURL(semconv.SchemaURL)`.
- **L3. Retry backoff has no jitter.** `uow.go:147` doubles deterministically,
  so concurrent workers that collide on a serialization failure retry in
  lock-step. Add jitter. (The shift also overflows past roughly 38 attempts;
  not a practical concern with the default of 3.)
- **L4. Stale import-skip entries.** `main.go:516-517` skip
  `<module>/log/v2` and `<module>/trace`, packages that do not exist in this
  module. Dead code from an earlier codebase.
- **L5. Dead branch in `slogAdapter.Log`.** `slog_adapter.go:61-66`: both arms
  of the `err`/`error` case produce `slog.Any("error", val)`.
- **L6. `toSnakeCase` splits acronyms.** `GetByID` becomes `get_by_i_d`
  (asserted by the test), so `HTTPClient` yields `h_t_t_p_client` as a metric
  subsystem. Consider acronym-aware splitting.
- **L7. `WithPGXTxOptions` incompatibility is detected per call.**
  `uow_db.go:250-254` reports the missing `BeginTx` at every transaction
  start. Detecting it in `NewPGXTransactor` (panic, or a `MustNew` variant)
  moves the failure to startup.
- **L8. Generic interfaces are not supported and not rejected.** `middlegen`
  ignores `TypeSpec.TypeParams`, so an interface with type parameters produces
  code that cannot compile, with no message pointing at the cause.
- **L9. Single context key assumes one database.** `db/uow_db.go:31-50` stores
  any `*sql.Tx` under one key and `db.Executor` hands it to any repository. A
  second `uow.Manager` over another database, nested inside an outer
  `RunWith`, attaches its tasks to the outer Unit of Work and runs them
  against the wrong connection. Document the single-database assumption.
- **L10. `endpoint`/`connectrpc` expose no header or trailer plumbing.** The
  generic `Endpoint` cannot read request headers or set response headers, so
  anything needing them must bypass the adapters. Fine as a limitation, worth
  a sentence in the README.

## Test coverage gaps

- `db`: no tests for `SQLXTransactor`, `XExecutor` (any branch),
  `WithSQLTxOptions`, `WithSQLXTxOptions`, or the `WithPGXTxOptions` error
  path, even though the last commit is titled as a sqlx named-query fix.
- `cmd/middlegen`: `TestGeneratedCodeCompiles` only proves compilation. None of
  H1, H2, M1 or M2 would be caught. Add (a) golden-file tests for each
  template so output changes are reviewed as diffs, and (b) a behavioural
  harness that wraps a spy implementation with the generated middleware and
  asserts forwarded arguments and returned values. The probe used for this
  review is under twenty lines per case.
- `uow`: `TestRunWith_ContextCancelled` accepts any error whose text contains
  "canceled"; assert on `errors.Is(err, context.Canceled)` only. There is no
  test that a retryable error raised by a *task* (not the commit) is retried,
  which is how Postgres reports most `40001` failures.
- `telemetry`: `TestInitTelemetry` leaves `slog.Default()` pointing at a
  shut-down OTel logger for the rest of the package. Restore it with
  `t.Cleanup` as `slog_adapter_test.go:150-151` already does.

## Documentation

- `README.md:66-71`: the repository example imports `errors` and never uses
  it, so the walkthrough does not compile as written.
- `README.md:26-36`: the directory tree omits `connectrpc/` and `endpoint/`.
  Section numbering starts at "4." with no sections 1 to 3.
- `README.md:288`: document the real deferred-return behaviour (see H2) and
  the transaction boundary (see H3).
- No retry evaluator ships for Postgres. A `db.IsRetryablePGError` that checks
  `pgconn.PgError.Code` for `40001` and `40P01` (and the equivalent for
  `lib/pq`) would make `WithRetryEvaluator` usable without every consumer
  rewriting it.

## CI

`.github/workflows/ci.yml` is minimal and correct. Cheap additions:

- `go mod tidy && git diff --exit-code go.mod go.sum` to keep the module file
  honest.
- `staticcheck` or `golangci-lint` (the `//nolint:staticcheck` comments in
  `main.go` suggest one was used at some point but is not enforced).
- `govulncheck ./...`.
- Dependabot or Renovate for the OTel and gRPC modules, which move quickly.

## Suggested order of work

1. H1, M1: extend `reservedParamNames` (one-line list change) and add a
   behavioural generator test. Smallest change, removes a silent wrong-value
   bug.
2. H2: return true zero values from deferred `uow_repo` methods.
3. H4: refuse or log in `uow_service` for error-less methods.
4. M3, M6: two small guards in `telemetry` and `db`.
5. H3: README rewrite of the transaction model, then decide whether
   `RunInTx` is worth adding.
6. M2: switch generator output through `imports.Process`.
7. M4, M5, then the low-severity items and CI additions as time allows.
