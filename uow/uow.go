// Package uow implements a driver-agnostic Unit of Work: a queue of deferred
// database tasks that a Manager executes inside a single transaction, with
// configurable retries for transient failures.
//
// # The model
//
// A boundary opened with [Manager.RunWith] runs a business action first,
// outside any transaction, with a context that carries a [UnitOfWork]. The
// action queues writes through [UnitOfWork.Defer] (the middlegen "uow_repo"
// wrappers do this for every decorated write). When the action returns, a
// transaction is opened, the queued tasks run inside it in order, and it
// commits. Nothing is opened when nothing was queued, and a retry re-runs
// the tasks only.
//
// A task is the unit of transactional code: it runs with the transaction
// open and no UnitOfWork in its context, so repository calls made from a
// task execute immediately, inside the transaction, and return real results.
// [Manager.RunInTx] runs one function as such a task, on its own when no
// boundary is open, or queued on the boundary the context carries.
//
// # Nesting
//
// Boundaries nest by joining what the context already carries:
//
//   - A UnitOfWork (a RunWith action): RunWith runs its action immediately
//     and the work it defers belongs to that unit. RunInTx queues its task on
//     that unit and returns nil at once; the task runs in the boundary's
//     transaction and its error is returned by the boundary.
//   - An open transaction but no UnitOfWork (a task): RunInTx runs its task
//     now. RunWith runs its action inside that transaction with a fresh unit
//     whose tasks run right after the action, and leaves the commit to the
//     outer boundary.
//
// [InTransaction] reports whether a context carries an open transaction. A
// context handed to an action or a task must not outlive its boundary.
//
// # Single database
//
// A context carries at most one UnitOfWork and, through the db package, one
// active transaction. Nesting RunWith/RunInTx calls that belong to different
// Managers (different databases) is not supported: the inner call joins the
// outer unit and its tasks would run against the outer transaction.
package uow

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

type uowKey struct{}

type txKey struct{}

// ErrLateDefer is returned when tasks were queued on a unit of work after it
// had started executing its tasks, typically by a goroutine that outlived the
// action. Those tasks never run; defer from the action instead. A task itself
// needs no Defer: the transaction is open, so it calls the repository directly.
var ErrLateDefer = errors.New("uow: Defer called while the unit of work was executing its tasks")

// Inject injects the Unit of Work into the context.
func Inject(ctx context.Context, uow *UnitOfWork) context.Context {
	return context.WithValue(ctx, uowKey{}, uow)
}

// Extract extracts the Unit of Work from the context.
func Extract(ctx context.Context) (*UnitOfWork, bool) {
	uow, ok := ctx.Value(uowKey{}).(*UnitOfWork)
	return uow, ok
}

// InTransaction reports whether ctx carries a database transaction opened by
// a Manager. It is true for the context tasks run with, RunInTx's included,
// and false inside a RunWith action.
// The Manager uses it to decide how a nested boundary joins; callers can use
// it to assert where their code runs.
func InTransaction(ctx context.Context) bool {
	return ctx.Value(txKey{}) == true
}

// markTransaction records on ctx that a transaction is open.
func markTransaction(ctx context.Context) context.Context {
	return context.WithValue(ctx, txKey{}, true)
}

// TaskFn defines the signature of a deferred task to be executed within a transaction.
type TaskFn func(ctx context.Context) error

// UnitOfWork queues tasks to be executed in a transactional batch.
// It is safe to Defer tasks from multiple goroutines while the action runs;
// the queued tasks themselves are executed sequentially in Defer order.
// Deferring after the action returned (from a task, or from a goroutine the
// action did not wait for) is reported as [ErrLateDefer].
type UnitOfWork struct {
	mu    sync.Mutex
	tasks []TaskFn
}

// NewUnitOfWork creates a new UnitOfWork.
func NewUnitOfWork() *UnitOfWork {
	return &UnitOfWork{tasks: make([]TaskFn, 0)}
}

// Defer queues a task for transactional execution.
func (uow *UnitOfWork) Defer(task TaskFn) {
	uow.mu.Lock()
	defer uow.mu.Unlock()
	uow.tasks = append(uow.tasks, task)
}

// snapshot returns a copy of the queued tasks safe to iterate without the lock.
func (uow *UnitOfWork) snapshot() []TaskFn {
	uow.mu.Lock()
	defer uow.mu.Unlock()
	tasks := make([]TaskFn, len(uow.tasks))
	copy(tasks, uow.tasks)
	return tasks
}

// count returns the number of tasks queued so far.
func (uow *UnitOfWork) count() int {
	uow.mu.Lock()
	defer uow.mu.Unlock()
	return len(uow.tasks)
}

// EvaluatorFn defines the signature of a function that determines if a database error is retryable.
type EvaluatorFn func(error) bool

// ActionFn defines the signature of a business action to be executed within a Unit of Work boundary.
type ActionFn func(uowCtx context.Context) error

// Option is a functional option for configuring Manager.
type Option func(*Manager)

// WithRetryEvaluator configures a custom function to determine if a database
// error is retryable. The evaluator receives the error as returned by the
// transaction attempt, which may wrap the driver error (commit failures are
// wrapped as "commit failed: ..."), so use errors.Is/errors.As rather than
// equality.
func WithRetryEvaluator(evaluator EvaluatorFn) Option {
	return func(m *Manager) {
		m.isRetryable = evaluator
	}
}

// WithMaxRetries configures the maximum retry attempts for transient
// transactional errors. Negative values are treated as 0 (no retries).
func WithMaxRetries(retries int) Option {
	return func(m *Manager) {
		m.maxRetries = max(retries, 0)
	}
}

// WithRetryDelay configures the retry backoff: the delay before retry n is
// baseDelay doubled n-1 times, capped at maxDelay, minus up to 25% random
// jitter so concurrent workers that collide do not retry in lock-step.
func WithRetryDelay(baseDelay, maxDelay time.Duration) Option {
	return func(m *Manager) {
		m.baseDelay = baseDelay
		m.maxDelay = maxDelay
	}
}

// Tx defines the generic commit and rollback contract.
type Tx interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Transactor defines the generic contract for beginning database transactions.
// BeginTx returns the transaction and a context derived from ctx that carries
// it, so executors resolved from that context run inside the transaction.
type Transactor interface {
	BeginTx(ctx context.Context) (Tx, context.Context, error)
}

// Manager manages transactional execution and retries for one database.
type Manager struct {
	db          Transactor
	maxRetries  int
	baseDelay   time.Duration
	maxDelay    time.Duration
	isRetryable EvaluatorFn
}

// NewManager creates a new Manager with the provided options. Without
// options no error is retried, which [WithRetryEvaluator] changes; the retry
// budget defaults to 3 retries, backing off from 50ms up to 500ms.
func NewManager(database Transactor, opts ...Option) *Manager {
	m := &Manager{
		db:          database,
		maxRetries:  3,
		baseDelay:   50 * time.Millisecond,
		maxDelay:    500 * time.Millisecond,
		isRetryable: func(error) bool { return false },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// RunWith runs a business action as a unit of work boundary:
//
//  1. action runs immediately with a context that carries a new UnitOfWork
//     but no database transaction. Repository reads made here go straight to
//     the connection pool.
//  2. If action returns nil and tasks were queued through [UnitOfWork.Defer],
//     a transaction is opened and the tasks run inside it, in order, followed
//     by a commit. No transaction is opened when nothing was deferred.
//  3. If a task or the commit fails with an error the retry evaluator accepts,
//     the queued tasks are re-run in a fresh transaction. action itself is not
//     re-run, so the closures execute with the values captured in step 1.
//
// The transaction and its isolation level therefore cover the deferred writes
// only. A check that a write depends on belongs in the write itself (a
// conditional UPDATE) or in a task, where the transaction is open; see
// [Manager.RunInTx].
//
// When ctx already carries a UnitOfWork, action joins it: it runs immediately
// and its deferred tasks are committed by the outer boundary. When ctx carries
// an open transaction but no UnitOfWork (inside a task), action runs in that
// transaction with a fresh unit whose tasks run right after it, and the outer
// boundary commits.
func (m *Manager) RunWith(ctx context.Context, action ActionFn) error {
	if _, ok := Extract(ctx); ok {
		return action(ctx)
	}
	if InTransaction(ctx) {
		return runJoined(ctx, action)
	}

	unit := NewUnitOfWork()
	if err := action(Inject(ctx, unit)); err != nil {
		return err
	}

	tasks := unit.snapshot()
	if len(tasks) == 0 {
		return nil // No writes deferred; bypass opening a transaction completely
	}

	return m.withRetry(ctx, func(ctx context.Context) error {
		return m.inTransaction(ctx, func(txCtx context.Context) error {
			return runTasks(txCtx, unit, tasks)
		})
	})
}

// RunInTx runs task as the single task of a unit of work:
//
//  1. A transaction is opened first and task runs with a context that carries
//     it. That context carries no UnitOfWork, so repository calls made through
//     the generated uow_repo middleware execute immediately, inside the
//     transaction, and return real results; reads run under the transaction's
//     isolation level.
//  2. The transaction is committed when task returns nil and rolled back when
//     it returns an error.
//  3. If task or the commit fails with an error the retry evaluator accepts,
//     task is re-run in a fresh transaction. It must therefore be safe to
//     repeat, which in practice means it only talks to the database.
//
// When ctx carries a UnitOfWork (inside a [Manager.RunWith] action), task is
// queued on it and RunInTx returns nil immediately: the task runs in that
// boundary's transaction, in Defer order, and its error is returned by the
// boundary. When ctx carries an open transaction and no UnitOfWork (inside a
// task), task runs now.
func (m *Manager) RunInTx(ctx context.Context, task TaskFn) error {
	if unit, ok := Extract(ctx); ok {
		unit.Defer(task)
		return nil
	}
	if InTransaction(ctx) {
		return task(ctx)
	}

	return m.withRetry(ctx, func(ctx context.Context) error {
		return m.inTransaction(ctx, task)
	})
}

// runJoined runs a RunWith action inside the open transaction txCtx carries,
// with a fresh unit of work, then runs the tasks the action deferred in the
// same transaction. Committing is left to whoever opened the transaction.
func runJoined(txCtx context.Context, action ActionFn) error {
	unit := NewUnitOfWork()
	if err := action(Inject(txCtx, unit)); err != nil {
		return err
	}
	return runTasks(txCtx, unit, unit.snapshot())
}

// withRetry runs attempt until it succeeds, returns a non-retryable error,
// the retry budget is exhausted, or ctx is done while waiting to retry.
func (m *Manager) withRetry(ctx context.Context, attempt func(ctx context.Context) error) error {
	var err error
	for n := 0; n <= m.maxRetries; n++ {
		if n > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.backoff(n)):
			}
		}

		err = attempt(ctx)
		if err == nil {
			return nil
		}

		if !m.isRetryable(err) {
			return err
		}
	}

	return fmt.Errorf("transaction failed after %d retries: %w", m.maxRetries, err)
}

// backoff returns the delay to wait before retry number n (n >= 1):
// baseDelay doubled n-1 times, capped at maxDelay, minus up to 25% jitter.
// The result never exceeds maxDelay and never overflows.
func (m *Manager) backoff(n int) time.Duration {
	delay := m.baseDelay
	for i := 1; i < n; i++ {
		if delay >= m.maxDelay/2 {
			delay = m.maxDelay
			break
		}
		delay *= 2
	}
	delay = min(delay, m.maxDelay)
	if delay <= 0 {
		return 0
	}
	// Uniform in [0.75*delay, delay].
	return delay - time.Duration(rand.Int64N(int64(delay/4)+1))
}

// inTransaction begins a transaction, runs body with the transactional
// context (marked so InTransaction reports true), and commits; any failure
// or panic rolls back.
func (m *Manager) inTransaction(ctx context.Context, body func(txCtx context.Context) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, txCtx, err := m.db.BeginTx(ctx)
	if err != nil {
		return err
	}
	txCtx = markTransaction(txCtx)

	var committed bool
	defer func() {
		// Roll back with a non-cancellable context so cleanup still reaches
		// the database when the caller's context has been cancelled.
		rollbackCtx := context.WithoutCancel(ctx)
		if r := recover(); r != nil {
			_ = tx.Rollback(rollbackCtx)
			panic(r)
		} else if !committed {
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := body(txCtx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}

	committed = true
	return nil
}

// runTasks executes tasks in order with txCtx, stopping at the first error
// or once txCtx is done. Tasks queued on unit while running would never
// execute, so their presence afterwards is reported as ErrLateDefer.
func runTasks(txCtx context.Context, unit *UnitOfWork, tasks []TaskFn) error {
	for _, task := range tasks {
		if err := txCtx.Err(); err != nil {
			return err
		}
		if err := task(txCtx); err != nil {
			return err
		}
	}
	if unit.count() > len(tasks) {
		return ErrLateDefer
	}
	return nil
}
