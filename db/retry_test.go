package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestIsRetryableTxError(t *testing.T) {
	assert.True(t, IsRetryableTxError(&pgconn.PgError{Code: "40001"}))
	assert.True(t, IsRetryableTxError(&pgconn.PgError{Code: "40P01"}))
	assert.True(t, IsRetryableTxError(fmt.Errorf("commit failed: %w", &pgconn.PgError{Code: "40001"})),
		"must see through wrapping added by the uow manager")
	assert.False(t, IsRetryableTxError(&pgconn.PgError{Code: "23505"}))
	assert.False(t, IsRetryableTxError(errors.New("plain")))
	assert.False(t, IsRetryableTxError(nil))
}
