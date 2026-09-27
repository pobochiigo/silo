package uow

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockTx struct {
	commitFunc    func(ctx context.Context) error
	rollbackFunc  func(ctx context.Context) error
	commitCalls   int32
	rollbackCalls int32
}

func (m *mockTx) Commit(ctx context.Context) error {
	atomic.AddInt32(&m.commitCalls, 1)
	if m.commitFunc != nil {
		return m.commitFunc(ctx)
	}
	return nil
}

func (m *mockTx) Rollback(ctx context.Context) error {
	atomic.AddInt32(&m.rollbackCalls, 1)
	if m.rollbackFunc != nil {
		return m.rollbackFunc(ctx)
	}
	return nil
}

type mockTransactor struct {
	beginTxFunc func(ctx context.Context) (Tx, context.Context, error)
	beginCalls  int32
}

func (m *mockTransactor) BeginTx(ctx context.Context) (Tx, context.Context, error) {
	atomic.AddInt32(&m.beginCalls, 1)
	if m.beginTxFunc != nil {
		return m.beginTxFunc(ctx)
	}
	return &mockTx{}, ctx, nil
}

func TestContextInjectExtract(t *testing.T) {
	ctx := context.Background()
	_, ok := Extract(ctx)
	assert.False(t, ok)

	uowInst := NewUnitOfWork()
	ctx = Inject(ctx, uowInst)
	extracted, ok := Extract(ctx)
	assert.True(t, ok)
	assert.Equal(t, uowInst, extracted)
}

func TestUnitOfWorkDefer(t *testing.T) {
	uowInst := NewUnitOfWork()
	assert.Empty(t, uowInst.tasks)

	called := false
	task := func(ctx context.Context) error {
		called = true
		return nil
	}

	uowInst.Defer(task)
	require.Len(t, uowInst.tasks, 1)

	err := uowInst.tasks[0](context.Background())
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestManagerOptions(t *testing.T) {
	transactor := &mockTransactor{}

	customEvaluator := func(err error) bool { return true }
	m := NewManager(
		transactor,
		WithMaxRetries(5),
		WithRetryEvaluator(customEvaluator),
		WithRetryDelay(10*time.Millisecond, 100*time.Millisecond),
	)

	assert.Equal(t, transactor, m.db)
	assert.Equal(t, 5, m.maxRetries)
	assert.Equal(t, 10*time.Millisecond, m.baseDelay)
	assert.Equal(t, 100*time.Millisecond, m.maxDelay)
	assert.True(t, m.isRetryable(errors.New("some error")))
}

func TestRunWith_NoTasks(t *testing.T) {
	transactor := &mockTransactor{}
	m := NewManager(transactor)

	actionCalled := false
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		actionCalled = true
		return nil
	})

	assert.NoError(t, err)
	assert.True(t, actionCalled)
	assert.Equal(t, int32(0), transactor.beginCalls)
}

func TestRunWith_ActionError(t *testing.T) {
	transactor := &mockTransactor{}
	m := NewManager(transactor)

	expectedErr := errors.New("action failed")
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			return nil
		})
		return expectedErr
	})

	assert.Equal(t, expectedErr, err)
	assert.Equal(t, int32(0), transactor.beginCalls)
}

func TestRunWith_SuccessWithTasks(t *testing.T) {
	tx := &mockTx{}
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return tx, ctx, nil
		},
	}
	m := NewManager(transactor)

	taskCalled := false
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			taskCalled = true
			return nil
		})
		return nil
	})

	assert.NoError(t, err)
	assert.True(t, taskCalled)
	assert.Equal(t, int32(1), transactor.beginCalls)
	assert.Equal(t, int32(1), tx.commitCalls)
	assert.Equal(t, int32(0), tx.rollbackCalls)
}

func TestRunWith_NestedCall(t *testing.T) {
	transactor := &mockTransactor{}
	m := NewManager(transactor)

	uowInst := NewUnitOfWork()
	ctx := Inject(context.Background(), uowInst)

	err := m.RunWith(ctx, func(uowCtx context.Context) error {
		extracted, ok := Extract(uowCtx)
		require.True(t, ok)
		assert.Equal(t, uowInst, extracted)
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, int32(0), transactor.beginCalls)
}

func TestRunWith_TaskErrorRollback(t *testing.T) {
	tx := &mockTx{}
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return tx, ctx, nil
		},
	}
	m := NewManager(transactor)

	expectedErr := errors.New("task failed")
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			return expectedErr
		})
		return nil
	})

	assert.Equal(t, expectedErr, err)
	assert.Equal(t, int32(1), transactor.beginCalls)
	assert.Equal(t, int32(0), tx.commitCalls)
	assert.Equal(t, int32(1), tx.rollbackCalls)
}

func TestRunWith_BeginTxError(t *testing.T) {
	expectedErr := errors.New("begin failed")
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return nil, nil, expectedErr
		},
	}
	m := NewManager(transactor)

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			return nil
		})
		return nil
	})

	assert.Equal(t, expectedErr, err)
	assert.Equal(t, int32(1), transactor.beginCalls)
}

func TestRunWith_CommitError(t *testing.T) {
	expectedErr := errors.New("commit failed")
	tx := &mockTx{
		commitFunc: func(ctx context.Context) error {
			return expectedErr
		},
	}
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return tx, ctx, nil
		},
	}
	m := NewManager(transactor)

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			return nil
		})
		return nil
	})

	assert.ErrorContains(t, err, "commit failed: commit failed")
	assert.Equal(t, int32(1), transactor.beginCalls)
	assert.Equal(t, int32(1), tx.commitCalls)
	assert.Equal(t, int32(1), tx.rollbackCalls) // Rolls back because committed is false
}

func TestRunWith_PanicRecovery(t *testing.T) {
	tx := &mockTx{}
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return tx, ctx, nil
		},
	}
	m := NewManager(transactor)

	assert.PanicsWithValue(t, "something went wrong", func() {
		_ = m.RunWith(context.Background(), func(uowCtx context.Context) error {
			uowInst, ok := Extract(uowCtx)
			require.True(t, ok)
			uowInst.Defer(func(ctx context.Context) error {
				panic("something went wrong")
			})
			return nil
		})
	})

	assert.Equal(t, int32(1), transactor.beginCalls)
	assert.Equal(t, int32(0), tx.commitCalls)
	assert.Equal(t, int32(1), tx.rollbackCalls)
}

func TestRunWith_RetryLogic(t *testing.T) {
	var beginCalls int32
	var commitCalls int32
	var rollbackCalls int32

	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			atomic.AddInt32(&beginCalls, 1)
			tx := &mockTx{
				commitFunc: func(ctx context.Context) error {
					atomic.AddInt32(&commitCalls, 1)
					if atomic.LoadInt32(&beginCalls) < 3 {
						return errors.New("transient error")
					}
					return nil
				},
				rollbackFunc: func(ctx context.Context) error {
					atomic.AddInt32(&rollbackCalls, 1)
					return nil
				},
			}
			return tx, ctx, nil
		},
	}

	m := NewManager(
		transactor,
		WithMaxRetries(3),
		WithRetryEvaluator(func(err error) bool {
			return err.Error() == "transient error" || strings.Contains(err.Error(), "transient error")
		}),
		WithRetryDelay(time.Millisecond, time.Millisecond),
	)

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			return nil
		})
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&beginCalls))
	assert.Equal(t, int32(3), atomic.LoadInt32(&commitCalls))
	assert.Equal(t, int32(2), atomic.LoadInt32(&rollbackCalls))
}

func TestRunWith_RetryExhausted(t *testing.T) {
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			tx := &mockTx{
				commitFunc: func(ctx context.Context) error {
					return errors.New("transient error")
				},
			}
			return tx, ctx, nil
		},
	}

	m := NewManager(
		transactor,
		WithMaxRetries(2),
		WithRetryEvaluator(func(err error) bool {
			return true
		}),
		WithRetryDelay(time.Millisecond, time.Millisecond),
	)

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			return nil
		})
		return nil
	})

	assert.ErrorContains(t, err, "transaction failed after 2 retries: commit failed: transient error")
}

func TestRunWith_ContextCancelled(t *testing.T) {
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			tx := &mockTx{
				commitFunc: func(ctx context.Context) error {
					return errors.New("transient error")
				},
			}
			return tx, ctx, nil
		},
	}

	m := NewManager(
		transactor,
		WithMaxRetries(5),
		WithRetryEvaluator(func(err error) bool {
			return true
		}),
		WithRetryDelay(50*time.Millisecond, 100*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())

	err := m.RunWith(ctx, func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			cancel() // cancel context during execution
			return nil
		})
		return nil
	})

	// Since we cancel inside, the retry loop will see the context done
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRunWith_TaskErrorIsRetried(t *testing.T) {
	// Postgres reports most serialization failures from the statement that
	// conflicts, not from COMMIT; a retryable error raised by a task must
	// re-run the batch in a fresh transaction.
	transient := errors.New("could not serialize access")
	var attempts int32

	transactor := &mockTransactor{}
	m := NewManager(
		transactor,
		WithMaxRetries(3),
		WithRetryEvaluator(func(err error) bool { return errors.Is(err, transient) }),
		WithRetryDelay(time.Millisecond, time.Millisecond),
	)

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		uowInst, ok := Extract(uowCtx)
		require.True(t, ok)
		uowInst.Defer(func(ctx context.Context) error {
			if atomic.AddInt32(&attempts, 1) < 3 {
				return transient
			}
			return nil
		})
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	assert.Equal(t, int32(3), atomic.LoadInt32(&transactor.beginCalls))
}

func TestRunInTx_TaskRunsInsideTransaction(t *testing.T) {
	type txKey struct{}
	tx := &mockTx{}
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return tx, context.WithValue(ctx, txKey{}, "tx"), nil
		},
	}
	m := NewManager(transactor)

	var ran bool
	err := m.RunInTx(context.Background(), func(txCtx context.Context) error {
		// The transaction is already open and visible to the task.
		assert.Equal(t, "tx", txCtx.Value(txKey{}))
		assert.True(t, InTransaction(txCtx))
		assert.Equal(t, int32(1), atomic.LoadInt32(&transactor.beginCalls))
		// A task carries no unit of work: decorated writes execute at once.
		_, ok := Extract(txCtx)
		assert.False(t, ok, "task context must not carry a unit of work")
		ran = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, ran)
	assert.Equal(t, int32(1), tx.commitCalls)
	assert.Equal(t, int32(0), tx.rollbackCalls)
}

func TestRunInTx_TaskErrorRollsBack(t *testing.T) {
	tx := &mockTx{}
	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return tx, ctx, nil
		},
	}
	m := NewManager(transactor)

	expectedErr := errors.New("task failed")
	err := m.RunInTx(context.Background(), func(context.Context) error {
		return expectedErr
	})

	assert.ErrorIs(t, err, expectedErr)
	assert.Equal(t, int32(1), transactor.beginCalls)
	assert.Equal(t, int32(0), tx.commitCalls)
	assert.Equal(t, int32(1), tx.rollbackCalls)
}

func TestRunInTx_RetryRerunsTheTask(t *testing.T) {
	transient := errors.New("could not serialize access")
	var taskRuns, commits int32

	transactor := &mockTransactor{
		beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
			return &mockTx{commitFunc: func(context.Context) error {
				if atomic.AddInt32(&commits, 1) < 2 {
					return transient
				}
				return nil
			}}, ctx, nil
		},
	}
	m := NewManager(
		transactor,
		WithMaxRetries(2),
		WithRetryEvaluator(func(err error) bool { return errors.Is(err, transient) }),
		WithRetryDelay(time.Millisecond, time.Millisecond),
	)

	err := m.RunInTx(context.Background(), func(context.Context) error {
		atomic.AddInt32(&taskRuns, 1)
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&taskRuns), "task re-runs on retry")
	assert.Equal(t, int32(2), atomic.LoadInt32(&transactor.beginCalls))
}

func TestRunInTx_InsideUnitIsQueued(t *testing.T) {
	transactor := &mockTransactor{}
	m := NewManager(transactor)

	// A unit injected by hand, as a RunWith action would carry: the task is
	// queued on it and nothing runs or opens until that boundary executes.
	unit := NewUnitOfWork()
	ctx := Inject(context.Background(), unit)

	var ran bool
	err := m.RunInTx(ctx, func(context.Context) error {
		ran = true
		return nil
	})

	require.NoError(t, err)
	assert.False(t, ran)
	assert.Equal(t, 1, unit.count(), "the task is queued on the unit in the context")
	assert.Equal(t, int32(0), transactor.beginCalls, "nested call must not open its own transaction")
}

func TestBackoff(t *testing.T) {
	m := NewManager(&mockTransactor{}, WithRetryDelay(100*time.Millisecond, time.Second))

	within := func(t *testing.T, got, expected time.Duration) {
		t.Helper()
		assert.GreaterOrEqual(t, got, expected*3/4, "lower jitter bound")
		assert.LessOrEqual(t, got, expected, "jitter never exceeds the computed delay")
	}

	for i := 0; i < 50; i++ {
		within(t, m.backoff(1), 100*time.Millisecond)
		within(t, m.backoff(2), 200*time.Millisecond)
		within(t, m.backoff(4), 800*time.Millisecond)
		within(t, m.backoff(5), time.Second)   // capped
		within(t, m.backoff(100), time.Second) // capped, no overflow
	}

	t.Run("huge max delay does not overflow", func(t *testing.T) {
		m := NewManager(&mockTransactor{}, WithRetryDelay(time.Second, time.Duration(1<<62)))
		got := m.backoff(200)
		assert.Greater(t, got, time.Duration(0))
		assert.LessOrEqual(t, got, time.Duration(1<<62))
	})

	t.Run("non-positive delays yield zero", func(t *testing.T) {
		m := NewManager(&mockTransactor{}, WithRetryDelay(0, 0))
		assert.Equal(t, time.Duration(0), m.backoff(3))
	})
}
