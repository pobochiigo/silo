// Command uow runs the ledger example against PostgreSQL with the driver
// family chosen by -driver. The service, the generated middlewares and the
// unit of work manager are the same for all three; only the repository and
// the transactor change.
//
//	DATABASE_URL=postgres://postgres:postgres@localhost:5432/silo?sslmode=disable go run . -driver pgx
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/jmoiron/sqlx"

	"github.com/pobochiigo/silo/db"
	"github.com/pobochiigo/silo/uow"

	"github.com/pobochiigo/silo/examples/internal/demo"
	"github.com/pobochiigo/silo/examples/uow/ledger"
	"github.com/pobochiigo/silo/examples/uow/ledger/pgxrepo"
	"github.com/pobochiigo/silo/examples/uow/ledger/sqlrepo"
	"github.com/pobochiigo/silo/examples/uow/ledger/sqlxrepo"
)

// backend is what differs between the driver families.
type backend struct {
	repo       ledger.Repository
	transactor uow.Transactor
	exec       func(ctx context.Context, sql string) error
	// count reads the number of entries with the driver's own pool, outside
	// the repository, so the final check does not rely on the code it checks.
	count func(ctx context.Context) (int64, error)
	close func()
}

func main() {
	driver := flag.String("driver", "pgx", "driver family: sql, sqlx or pgx")
	workers := flag.Int("workers", 8, "concurrent transfer workers")
	transfers := flag.Int("transfers", 25, "transfers per worker")
	verbose := flag.Bool("v", false, "debug logging: shows the generated middlewares' started lines")
	flag.Parse()

	ctx, stop := demo.Context()
	defer stop()
	shutdown := demo.Telemetry(ctx, "silo-example-uow-"+*driver, *verbose)
	defer shutdown()

	dsn := demo.Env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/silo?sslmode=disable")
	be, err := open(ctx, *driver, dsn)
	if err != nil {
		demo.Fail("connect", err)
	}
	defer be.close()
	if err := be.exec(ctx, ledger.Schema); err != nil {
		demo.Fail("schema", err)
	}
	if err := be.exec(ctx, `TRUNCATE entries, accounts`); err != nil {
		demo.Fail("reset", err)
	}

	// The manager retries serialization failures and deadlocks; the evaluator
	// counts them so the run can report how often SERIALIZABLE pushed back.
	var retries atomic.Int64
	manager := uow.NewManager(be.transactor,
		uow.WithRetryEvaluator(func(err error) bool {
			if db.IsRetryableTxError(err) {
				retries.Add(1)
				return true
			}
			return false
		}),
		uow.WithMaxRetries(25),
		uow.WithRetryDelay(2*time.Millisecond, 50*time.Millisecond),
	)

	// Decorators are applied inside out: the UoW middleware sits closest to
	// the implementation so logging and tracing see the real calls.
	repo := ledger.RepositoryUoWMiddleware()(be.repo)
	repo = ledger.RepositoryMetricsMiddleware()(repo)
	repo = ledger.RepositoryTracingMiddleware()(repo)
	repo = ledger.RepositoryLoggingMiddleware()(repo)

	svc := ledger.ServiceUoWMiddleware(manager)(ledger.NewService(repo))
	svc = ledger.ServiceTracingMiddleware()(svc)
	svc = ledger.ServiceLoggingMiddleware()(svc)

	demo.Step(1, "OpenAccount runs in RunWith: CreateAccount and AddEntry are queued, then committed in one transaction")
	alice, err := svc.OpenAccount(ctx, "alice", "Alice", 10_000)
	if err != nil {
		demo.Fail("open alice", err)
	}
	if _, err := svc.OpenAccount(ctx, "bob", "Bob", 10_000); err != nil {
		demo.Fail("open bob", err)
	}
	fmt.Printf("   alice opened with %d; the *Account the service returned is the one it passed in\n", alice.Balance)

	demo.Step(2, "Transfer runs in RunWith with the check in the write: the debit statement refuses an overdraft, the unit of work rolls back, nothing is written")
	err = svc.Transfer(ctx, "alice", "bob", 1_000_000)
	fmt.Printf("   transfer of 1000000 -> %v\n", err)
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		demo.Fail("expected ErrInsufficientFunds", err)
	}
	printBalances(ctx, svc)

	demo.Step(3, fmt.Sprintf("%d workers x %d transfers between the two accounts under SERIALIZABLE", *workers, *transfers))
	start := time.Now()
	var ok, insufficient atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < *transfers; i++ {
				from, to := "alice", "bob"
				if (w+i)%2 == 1 {
					from, to = to, from
				}
				err := svc.Transfer(ctx, from, to, 1+rand.Int64N(500))
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ledger.ErrInsufficientFunds):
					insufficient.Add(1)
				default:
					demo.Fail("transfer", err)
				}
			}
		}(w)
	}
	wg.Wait()
	fmt.Printf("   %d transfers committed, %d refused for insufficient funds, %d transactions retried after a serialization failure or deadlock, %s\n",
		ok.Load(), insufficient.Load(), retries.Load(), time.Since(start).Round(time.Millisecond))

	demo.Step(4, "Invariants: money was only moved, and every transfer left exactly two entries")
	total := printBalances(ctx, svc)
	entries, err := be.count(ctx)
	if err != nil {
		demo.Fail("count entries", err)
	}
	fmt.Printf("   total balance %d (want 20000), entries %d (want %d)\n", total, entries, 2+2*ok.Load())
	if total != 20_000 || entries != 2+2*ok.Load() {
		demo.Fail("invariants", errors.New("the ledger is inconsistent"))
	}

	demo.Step(5, "ApplyInterest runs as one task (//middlegen:in-tx): BEGIN first, ListAccounts inside the transaction, the writes execute at once, then COMMIT")
	before := balances(ctx, svc)
	if err := svc.ApplyInterest(ctx, 100); err != nil {
		demo.Fail("apply interest", err)
	}
	printBalances(ctx, svc)
	expectInterest(ctx, svc, before, 100)

	demo.Step(6, "Nesting: an in-tx method called inside RunWith is queued and runs with that boundary's transaction; inside a task it runs at once")
	before = balances(ctx, svc)
	err = manager.RunWith(ctx, func(ctx context.Context) error {
		if err := svc.ApplyInterest(ctx, 100); err != nil { // queued on the RunWith unit of work
			return err
		}
		acc, err := repo.GetAccount(ctx, "alice") // a read on the pool: the task has not run yet
		if err != nil {
			return err
		}
		fmt.Printf("   inside the RunWith action, after the call: alice %d (unchanged, the task is queued)\n", acc.Balance)
		return nil
	})
	if err != nil {
		demo.Fail("in-tx inside RunWith", err)
	}
	fmt.Printf("   after the boundary committed:                alice %d\n", balances(ctx, svc)["alice"])
	expectInterest(ctx, svc, before, 100)

	before = balances(ctx, svc)
	err = manager.RunInTx(ctx, func(txCtx context.Context) error {
		return svc.ApplyInterest(txCtx, 100) // inside a task: runs now, in the open transaction
	})
	if err != nil {
		demo.Fail("in-tx inside RunInTx", err)
	}
	fmt.Printf("   inside a RunInTx task: ran at once, alice %d\n", balances(ctx, svc)["alice"])
	expectInterest(ctx, svc, before, 100)

	demo.Step(7, "Statement is a read-only RunWith: nothing deferred, no transaction opened")
	acc, latest, err := svc.Statement(ctx, "alice")
	if err != nil {
		demo.Fail("statement", err)
	}
	fmt.Printf("   %s (%s) balance %d, latest entries:\n", acc.ID, acc.Owner, acc.Balance)
	for _, e := range latest {
		fmt.Printf("   %6d  %+7d  %s\n", e.ID, e.Amount, e.Memo)
	}
	slog.Info("done", slog.String("driver", *driver))
}

func printBalances(ctx context.Context, svc ledger.Service) int64 {
	var total int64
	for _, id := range []string{"alice", "bob"} {
		acc, _, err := svc.Statement(ctx, id)
		if err != nil {
			demo.Fail("statement "+id, err)
		}
		fmt.Printf("   %-6s %6d\n", acc.ID, acc.Balance)
		total += acc.Balance
	}
	return total
}

// balances reads both accounts through the service.
func balances(ctx context.Context, svc ledger.Service) map[string]int64 {
	out := map[string]int64{}
	for _, id := range []string{"alice", "bob"} {
		acc, _, err := svc.Statement(ctx, id)
		if err != nil {
			demo.Fail("statement "+id, err)
		}
		out[id] = acc.Balance
	}
	return out
}

// expectInterest checks that every balance grew by rateBps of its value in
// before, the way ApplyInterest computes it.
func expectInterest(ctx context.Context, svc ledger.Service, before map[string]int64, rateBps int64) {
	for id, got := range balances(ctx, svc) {
		if want := before[id] + before[id]*rateBps/10_000; got != want {
			demo.Fail("interest", fmt.Errorf("%s holds %d after interest, want %d", id, got, want))
		}
	}
}

// open connects with the requested driver family and returns the pieces that
// differ: the repository implementation and the matching transactor, both
// configured for SERIALIZABLE transactions.
func open(ctx context.Context, driver, dsn string) (*backend, error) {
	serializable := &sql.TxOptions{Isolation: sql.LevelSerializable}
	switch driver {
	case "sql":
		pool, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		if err := pool.PingContext(ctx); err != nil {
			return nil, err
		}
		return &backend{
			repo:       sqlrepo.New(pool),
			transactor: db.NewSQLTransactor(pool, db.WithSQLTxOptions(serializable)),
			exec:       func(ctx context.Context, q string) error { _, err := pool.ExecContext(ctx, q); return err },
			count: func(ctx context.Context) (n int64, err error) {
				return n, pool.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&n)
			},
			close: func() { _ = pool.Close() },
		}, nil
	case "sqlx":
		pool, err := sqlx.ConnectContext(ctx, "pgx", dsn)
		if err != nil {
			return nil, err
		}
		return &backend{
			repo:       sqlxrepo.New(pool),
			transactor: db.NewSQLXTransactor(pool, db.WithSQLXTxOptions(serializable)),
			exec:       func(ctx context.Context, q string) error { _, err := pool.ExecContext(ctx, q); return err },
			count: func(ctx context.Context) (n int64, err error) {
				return n, pool.GetContext(ctx, &n, `SELECT count(*) FROM entries`)
			},
			close: func() { _ = pool.Close() },
		}, nil
	case "pgx":
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return nil, err
		}
		if err := pool.Ping(ctx); err != nil {
			return nil, err
		}
		return &backend{
			repo:       pgxrepo.New(pool),
			transactor: db.NewPGXTransactor(pool, db.WithPGXTxOptions(pgx.TxOptions{IsoLevel: pgx.Serializable})),
			exec:       func(ctx context.Context, q string) error { _, err := pool.Exec(ctx, q); return err },
			count: func(ctx context.Context) (n int64, err error) {
				return n, pool.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&n)
			},
			close: pool.Close,
		}, nil
	default:
		return nil, fmt.Errorf("unknown driver %q (want sql, sqlx or pgx)", driver)
	}
}
