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
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "canceled"))
}
