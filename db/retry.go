package db

import "errors"

// retryableSQLStates lists the PostgreSQL SQLSTATE codes that indicate a
// transaction failed only because of concurrent activity and can be retried
// as-is: 40001 serialization_failure and 40P01 deadlock_detected.
var retryableSQLStates = map[string]bool{
	"40001": true,
	"40P01": true,
}

// IsRetryableTxError reports whether err (or any error it wraps) carries a
// PostgreSQL SQLSTATE that marks the transaction as retryable: 40001
// (serialization_failure) or 40P01 (deadlock_detected). It works with any
// driver whose errors expose SQLState() string, which includes pgx
// (*pgconn.PgError) and lib/pq (*pq.Error). Pass it to
// uow.WithRetryEvaluator to retry serialization failures automatically.
func IsRetryableTxError(err error) bool {
	var stateErr interface{ SQLState() string }
	if errors.As(err, &stateErr) {
		return retryableSQLStates[stateErr.SQLState()]
	}
	return false
}
