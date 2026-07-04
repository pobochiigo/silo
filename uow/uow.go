package uow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type uowKey struct{}

// Inject injects the Unit of Work into the context.
func Inject(ctx context.Context, uow *UnitOfWork) context.Context {
	return context.WithValue(ctx, uowKey{}, uow)
}

// Extract extracts the Unit of Work from the context.
func Extract(ctx context.Context) (*UnitOfWork, bool) {
	uow, ok := ctx.Value(uowKey{}).(*UnitOfWork)
	return uow, ok
}

// TaskFn defines the signature of a deferred task to be executed within a transaction.
type TaskFn func(ctx context.Context) error

// UnitOfWork queues tasks to be executed in a transactional batch.
// It is safe to Defer tasks from multiple goroutines; the queued tasks
// themselves are executed sequentially in Defer order.
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

// EvaluatorFn defines the signature of a function that determines if a database error is retryable.
type EvaluatorFn func(error) bool

// ActionFn defines the signature of a business action to be executed within a Unit of Work boundary.
type ActionFn func(uowCtx context.Context) error

// Option is a functional option for configuring Manager.
type Option func(*Manager)

// WithRetryEvaluator configures a custom function to determine if a database error is retryable.
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

// WithRetryDelay configures the retry delay backoff parameters.
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
type Transactor interface {
	BeginTx(ctx context.Context) (Tx, context.Context, error)
}

// Manager manages transactional execution and retries.
type Manager struct {
	db          Transactor
	maxRetries  int
	baseDelay   time.Duration
	maxDelay    time.Duration
	isRetryable EvaluatorFn
}

// NewManager creates a new Manager with the provided options.
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

// RunWith runs a business action within a Unit of Work boundary.
func (m *Manager) RunWith(ctx context.Context, action ActionFn) error {
	if _, ok := Extract(ctx); ok {
		return action(ctx)
	}

	uow := NewUnitOfWork()
	uowCtx := Inject(ctx, uow)

	if err := action(uowCtx); err != nil {
		return err
	}

	tasks := uow.snapshot()
	if len(tasks) == 0 {
		return nil // No writes deferred; bypass opening a transaction completely
	}

	return m.commitWithRetry(ctx, tasks)
}

func (m *Manager) commitWithRetry(ctx context.Context, tasks []TaskFn) error {
	var err error
	for attempt := 0; attempt <= m.maxRetries; attempt++ {
		if attempt > 0 {
			delay := min(m.baseDelay*(1<<uint(attempt-1)), m.maxDelay)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err = m.executeTransaction(ctx, tasks)
		if err == nil {
			return nil
		}

		if !m.isRetryable(err) {
			return err
		}
	}

	return fmt.Errorf("transaction failed after %d retries: %w", m.maxRetries, err)
}

func (m *Manager) executeTransaction(ctx context.Context, tasks []TaskFn) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, txCtx, err := m.db.BeginTx(ctx)
	if err != nil {
		return err
	}

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

	for _, task := range tasks {
		if err := txCtx.Err(); err != nil {
			return err
		}
		if err := task(txCtx); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}

	committed = true
	return nil
}
