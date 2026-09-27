package uow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type probeTxKey struct{}

// markingTransactor stamps the contexts it returns like the db transactors
// do, so tests can tell where the transaction is visible.
func markingTransactor() *mockTransactor {
	return &mockTransactor{beginTxFunc: func(ctx context.Context) (Tx, context.Context, error) {
		return &mockTx{}, context.WithValue(ctx, probeTxKey{}, "tx"), nil
	}}
}

func TestInTransaction(t *testing.T) {
	m := NewManager(markingTransactor())

	assert.False(t, InTransaction(context.Background()))

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		assert.False(t, InTransaction(uowCtx), "a RunWith action runs before the transaction is opened")
		unit, _ := Extract(uowCtx)
		unit.Defer(func(txCtx context.Context) error {
			assert.True(t, InTransaction(txCtx), "tasks run inside the transaction")
			return nil
		})
		return nil
	})
	require.NoError(t, err)

	err = m.RunInTx(context.Background(), func(txCtx context.Context) error {
		assert.True(t, InTransaction(txCtx), "a RunInTx action runs inside the transaction")
		return nil
	})
	require.NoError(t, err)
}

func TestRunInTx_InsideRunWithActionIsRefused(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor)

	var innerRan bool
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		return m.RunInTx(uowCtx, func(context.Context) error {
			innerRan = true
			return nil
		})
	})

	assert.ErrorIs(t, err, ErrNoTransaction)
	assert.False(t, innerRan, "the inner action must not run without the transaction it asked for")
	assert.Equal(t, int32(0), transactor.beginCalls)
}

func TestRunInTx_InsideRunInTxActionJoins(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor)

	var order []string
	err := m.RunInTx(context.Background(), func(outerCtx context.Context) error {
		outer, _ := Extract(outerCtx)
		return m.RunInTx(outerCtx, func(innerCtx context.Context) error {
			inner, _ := Extract(innerCtx)
			assert.Same(t, outer, inner, "the inner call shares the outer unit")
			assert.Equal(t, "tx", innerCtx.Value(probeTxKey{}))
			inner.Defer(func(context.Context) error { order = append(order, "task"); return nil })
			order = append(order, "inner")
			return nil
		})
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"inner", "task"}, order, "deferred work runs once, after the outer action")
	assert.Equal(t, int32(1), transactor.beginCalls)
}

func TestRunWith_InsideRunInTxActionJoins(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor)

	var taskRan bool
	err := m.RunInTx(context.Background(), func(outerCtx context.Context) error {
		return m.RunWith(outerCtx, func(innerCtx context.Context) error {
			assert.True(t, InTransaction(innerCtx))
			unit, _ := Extract(innerCtx)
			unit.Defer(func(context.Context) error { taskRan = true; return nil })
			return nil
		})
	})

	require.NoError(t, err)
	assert.True(t, taskRan)
	assert.Equal(t, int32(1), transactor.beginCalls)
}

func TestRunWith_InsideTaskJoinsTheOpenTransaction(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor)

	var order []string
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		unit, _ := Extract(uowCtx)
		unit.Defer(func(txCtx context.Context) error {
			order = append(order, "outer task")
			return m.RunWith(txCtx, func(innerCtx context.Context) error {
				assert.Equal(t, "tx", innerCtx.Value(probeTxKey{}), "the inner action sees the open transaction")
				assert.True(t, InTransaction(innerCtx))
				inner, _ := Extract(innerCtx)
				inner.Defer(func(context.Context) error { order = append(order, "inner task"); return nil })
				order = append(order, "inner action")
				return nil
			})
		})
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"outer task", "inner action", "inner task"}, order)
	assert.Equal(t, int32(1), transactor.beginCalls, "no second transaction may be opened inside the first")
}

func TestRunInTx_InsideTaskJoinsTheOpenTransaction(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor)

	var innerRan bool
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		unit, _ := Extract(uowCtx)
		unit.Defer(func(txCtx context.Context) error {
			return m.RunInTx(txCtx, func(innerCtx context.Context) error {
				innerRan = InTransaction(innerCtx)
				return nil
			})
		})
		return nil
	})

	require.NoError(t, err)
	assert.True(t, innerRan)
	assert.Equal(t, int32(1), transactor.beginCalls)
}

func TestRunWith_JoinedActionErrorPropagates(t *testing.T) {
	m := NewManager(markingTransactor())
	boom := errors.New("boom")

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		unit, _ := Extract(uowCtx)
		unit.Defer(func(txCtx context.Context) error {
			return m.RunWith(txCtx, func(context.Context) error { return boom })
		})
		return nil
	})

	assert.ErrorIs(t, err, boom)
}

func TestRunWith_TaskDeferringTaskIsReported(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor)

	var lateRan bool
	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		unit, _ := Extract(uowCtx)
		unit.Defer(func(context.Context) error {
			unit.Defer(func(context.Context) error { lateRan = true; return nil })
			return nil
		})
		return nil
	})

	assert.ErrorIs(t, err, ErrLateDefer)
	assert.False(t, lateRan)
	assert.Equal(t, int32(1), transactor.beginCalls)
}

func TestRunInTx_TaskDeferringTaskIsReported(t *testing.T) {
	m := NewManager(markingTransactor())

	err := m.RunInTx(context.Background(), func(txCtx context.Context) error {
		unit, _ := Extract(txCtx)
		unit.Defer(func(context.Context) error {
			unit.Defer(func(context.Context) error { return nil })
			return nil
		})
		return nil
	})

	assert.ErrorIs(t, err, ErrLateDefer)
}

func TestRunWith_LateDeferIsNotRetried(t *testing.T) {
	transactor := markingTransactor()
	m := NewManager(transactor,
		WithMaxRetries(3),
		WithRetryEvaluator(func(err error) bool { return !errors.Is(err, ErrLateDefer) }),
	)
	var runs int32

	err := m.RunWith(context.Background(), func(uowCtx context.Context) error {
		unit, _ := Extract(uowCtx)
		unit.Defer(func(context.Context) error {
			atomic.AddInt32(&runs, 1)
			unit.Defer(func(context.Context) error { return nil })
			return nil
		})
		return nil
	})

	assert.ErrorIs(t, err, ErrLateDefer)
	assert.Equal(t, int32(1), atomic.LoadInt32(&runs))
}
