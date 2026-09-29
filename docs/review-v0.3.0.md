# Repository review: v0.3.0

Third full review of `github.com/pobochiigo/silo`, at the v0.3.0 tag plus the
flow-comparison documentation (`main` at 34266d3, 2026-09-28). The previous
reviews are in [repository-review.md](repository-review.md) and
[review-v0.1.0.md](review-v0.1.0.md). Scope: every non-test Go file, the
tests, the generator templates and golden files, the README, the examples
module and both workflows.

Every finding marked **verified** was reproduced with a test written against
the code as it was, and is fixed on this branch.

## Summary

The single unit-of-work model introduced in v0.3.0 holds up: the two rules
(a call made with a unit of work in the context is queued on it, a call made
with a transaction and no unit runs now) are implemented in one place each,
pinned by `uow/nesting_test.go` for all four service-to-service shapes, and
the `db` executors, the generator templates and the examples agree with them.
`gofmt`, `go vet`, staticcheck, govulncheck, `go test -race ./...`, the
tidy and `go generate` diff checks and all four examples pass. The findings
are at the edges: one dial target, one missing adapter, and documentation.

| # | Severity | Finding | Status |
|---|---|---|---|
| M1 | Medium | `telemetry.Config.Endpoint` given as a bare host (`"alloy"`) dialled port 443. | Fixed: defaults to 4317 like the URL form. **Verified.** |
| M2 | Medium | Typed endpoints had no observability: the go-kit middlewares could not decorate an `endpoint.Endpoint[Req, Resp]`. | Fixed: `telemetry.Adapt`. |
| L1 | Low | The `in-tx` signature rule was enforced whatever kind was generated, against the documented rule that directives of other kinds are ignored. | Fixed: checked for `uow_service` only. **Verified.** |
| L2 | Low | The flow comparison did not say what `B`'s own logging and tracing decorators see in shape 2. | Documented. |
| L3 | Low | The README pinned `middlegen@v0.1.0` in the `go run` form; that generator predates `in-tx`. | Fixed: `v0.3.0`. |
| L4 | Low | The manager's retry defaults, and that nothing is retried without an evaluator, were only visible in the code. | Documented in `NewManager` and the README. |

## Findings

### M1. A bare host in `Endpoint` was dialled on port 443 (verified)

`Config.Endpoint` accepts `host:port` or a URL. The URL branch filled a
missing port with 4317; the `host:port` branch passed the string through,
so `Endpoint: "alloy"` reached `grpc.NewClient("alloy")`, whose DNS resolver
defaults the port to 443. The exporters then failed to connect with no hint
that the port was the reason, since 443 is where a TLS collector could well
listen.

**Fix.** Both forms go through one helper that appends 4317 when
`net.SplitHostPort` finds no port, stripping IPv6 brackets first so `[::1]`
becomes `[::1]:4317` and not `[[::1]]:4317`. Targets with a port, including
`unix:` style ones, are untouched.
Tests: `TestConfigExporterTarget` (four new cases).

### M2. No middleware for typed endpoints

The `endpoint` package offers `Endpoint[Req, Resp]` and the `connectrpc`
package adapts it to Connect handlers and clients, but the tracing, logging
and metrics middlewares in `telemetry` are written for go-kit's untyped
`endpoint.Endpoint`. A typed endpoint could only be observed by hand-written
decorators, which is what the connectrpc example did with `timing`.

**Fix.** `telemetry.Adapt[Req, Resp](mw)` lifts a go-kit middleware to a
`middleware.Middleware[endpoint.Endpoint[Req, Resp]]`. The go-kit middleware
sees the request and response as `any`; one that replaces either with a
value of another type makes the adapted endpoint return an error rather than
panic on the type assertion, and nil interface values pass through. The type
parameters must be spelled out, since Go cannot infer them from an untyped
middleware.

Follow-up in the same change: the tracing, logging and metrics middlewares
are typed first, `Tracing`, `Logging` and `Metrics` over
`endpoint.Endpoint[Req, Resp]`, since none of them looks at the request or
the response. go-kit's endpoint is `Endpoint[any, any]` under another name,
so `telemetry.Kit` converts the `[any, any]` instantiation into a go-kit
middleware by type conversion, and `Adapt` stays for foreign go-kit
middlewares. `Metrics` records on a `MetricsRecorder`, so the series carry
the caller's subsystem (`auth_endpoint_requests_total`) instead of `gokit_`;
the deprecated `MetricsMiddleware` records under the subsystem `endpoint`.
`middleware.Chain` composes middlewares of one type. The connectrpc example
decorates its `Endpoints` fields with `telemetry.Metrics` and both `Service`
sides with `middleware.Chain`.

The adapter lives in `telemetry`, not in `endpoint`, on purpose. A first cut
put it in `endpoint`, which made that package import go-kit; the `bhole`
SDK, which depends on `silo/endpoint` and `silo/connectrpc` only, then
failed to build until `go mod tidy` added `github.com/go-kit/kit` and two
genproto modules to its `go.mod`. The `endpoint` package stays standard
library only, and the package doc says so.
Tests: `telemetry/adapt_test.go`, including composition with
`LoggingMiddleware`.

### L1. The `in-tx` rule fired for every kind (verified)

`buildMethod` rejected an `in-tx` method that does not take a context and
return only an error, whatever `-kinds` asked for. The README promises that
"directives for kinds that are not being generated are ignored", so
generating `logging` alone for such an interface must not fail on a
`uow_service` constraint.

**Fix.** The check moved next to the other `uow_service`-only diagnostic in
`run` and applies when that kind is requested. The error text is unchanged.
Test: `TestRunValidation`.

### L2, L3, L4. Documentation

Shape 2 of the flow comparison now states that the logging and tracing
middlewares around `B` see only the queuing call: `B`'s span ends before its
body runs, the body's repository calls are traced under `A`'s span, and a
failure is logged as `A`'s. This follows from the generated `uow_service`
middleware sitting closest to the implementation and the queued task calling
the implementation directly with the boundary's transactional context.

The `go run ...@version` form in the README names v0.3.0, the first version
whose generator knows `in-tx`. `NewManager` documents that no error is
retried without `WithRetryEvaluator` and that the budget defaults to 3
retries backing off from 50ms to 500ms; the README repeats it next to the
retry example. The `Endpoint` row of the configuration table and the `in-tx`
row of the directive table carry the M1 and L1 behaviour.

## Checked and left as is

- **`uow`.** `RunWith` joins a unit in the context, joins an open transaction
  through `runJoined` with a fresh unit, and otherwise runs the action before
  opening anything; `RunInTx` queues on a unit, runs now inside a transaction,
  and otherwise opens one. `runTasks` reports late `Defer` calls against the
  unit it executes, so a fresh inner unit never trips the outer check.
  Retries re-run the queued tasks or the whole task, never the action; the
  backoff is capped and jittered; rollback uses a non-cancellable context and
  re-panics after rolling back.
- **`db`.** The executors return the transaction of their own driver family,
  panic on the other family's, and fall back to the pool otherwise;
  `XExecutor`'s dynamic `*sql.Tx` wrap always carries a mapper. `NewTLS(nil)`
  in `dialCollector` is safe: gRPC clones a nil config into an empty one.
- **`telemetry`.** Options from `Config` override the OTLP environment
  variables only when set; the shared connection carries compression and
  headers per call; the fan-out handler clones records per destination; the
  go-kit adapter resolves `slog.Default()` lazily.
- **`middlegen`.** Template imports are pruned per interface through the
  fixed-import tables; reserved parameter names and package qualifiers are
  renamed; echo resolution and its nil guard match the README; embedded
  interfaces from any package are expanded with their directives.
- **Examples and CI.** The examples module regenerates without a diff, all
  four programs run, and the workflows match the README's description of
  releasing.

## Verification

```
gofmt -l . && go vet ./... && go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
cd examples && go mod tidy && go generate ./... && git diff --exit-code
cd examples && go run ./middlegen && go run ./telemetry && go run ./connectrpc
```
