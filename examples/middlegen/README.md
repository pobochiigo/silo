# middlegen: every directive, no infrastructure

An in-memory inventory decorated with all four middleware kinds. The
repository prints each write when it really executes and the transactor prints
`BEGIN`, `COMMIT` and `ROLLBACK`, so the output shows when deferred calls run.
Spans are printed one per line as they end, and the metrics are read back
in-process at the end.

```bash
go run ./middlegen
```

## Where each directive is

`inventory/inventory.go`:

| Method | Directive | Effect in the output |
|---|---|---|
| `Reader.Get`, `Reader.List` | `//middlegen:non-transactional` on an embedded interface | Run immediately, before any `BEGIN`. |
| `Save` | `//middlegen:metric counter:inventory_saves_total`, `//middlegen:metric attr:stock=stockOf(item)` | `inventory_saves_total` and a `stock` attribute on the request metrics. |
| `Merge` | `//middlegen:echo into` | Returns the `into` parameter while queued. |
| `Archive` | `//middlegen:echo none` | Returns `nil` while queued. |
| `RotateKey` | `//middlegen:redact key` | Logged as `key=[REDACTED]`. |
| `Decrement` | none | Basic-typed results are never echoed: returns `0` while queued. |
| `io.Closer` | none | No context: logged and measured, not traced or deferred. |
| `Service.Reserve` | `//middlegen:in-tx` | Wrapped in `RunInTx`: `BEGIN` first, the read runs inside the transaction, the queued write runs before `COMMIT`. |

`Service` is decorated from `inventory/svcmw`, a separate package, with
`-dir=middlegen/inventory`: the generated code imports `inventory` and refers
to `inventory.Service`.

## Reading the output

Step 1, `RunWith`: the read happens during the method, the write after it.

```
level=DEBUG msg="Restock started" service=inventory sku=widget qty=10
level=DEBUG msg="Get started" service=inventory sku=widget
level=DEBUG msg="Save started" service=inventory item="&{SKU:widget Name:widget Quantity:10 Reserved:0}"
   [span] inventory.Save             13µs  ok
   [tx] BEGIN
   [repo] Save executed: widget quantity=10 reserved=0
   [tx] COMMIT
   [span] inventory.Restock          91µs  ok
```

Step 2, `RunInTx` through `//middlegen:in-tx`: `BEGIN` comes first, the read
runs inside the transaction, and the queued write still runs after the
method body, just before `COMMIT`. Same single write phase, different
snapshot for the read.

```
level=DEBUG msg="Reserve started" service=inventory sku=widget qty=3
   [tx] BEGIN
level=DEBUG msg="Get started" service=inventory sku=widget
level=DEBUG msg="Save started" service=inventory item="&{SKU:widget Name:widget Quantity:10 Reserved:3}"
   [span] inventory.Save              4µs  ok
   [repo] Save executed: widget quantity=10 reserved=3
   [tx] COMMIT
```

Step 4 queues five writes in one `RunWith` and prints what each returned
before the transaction opens; step 5 fails inside `RunInTx` and shows the
`ROLLBACK`, the span marked with the error and the `Reserve failed` log line.
Step 7 prints the instruments the metrics middleware created:

```
   repository_requests_total{method=Save,stock=stocked} = 4
   repository_errors_total{method=Get} = 1
   repository_request_duration_seconds{method=Get} count=3
   inventory_saves_total{} = 4
```
