package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pocketbase/dbx"
)

// default retries intervals (in ms)
var defaultRetryIntervals = []int{50, 100, 150, 200, 300, 400, 500, 700, 1000}

// default max retry attempts
const defaultMaxLockRetries = 12

// Retryable Postgres SQLSTATE codes.
const (
	pgCodeSerializationFailure = "40001"
	pgCodeDeadlockDetected     = "40P01"
	// class 08 - Connection Exception is also worth retrying for
	// transient network blips.
	pgCodeConnectionException       = "08000"
	pgCodeConnectionDoesNotExist    = "08003"
	pgCodeConnectionFailure         = "08006"
	pgCodeAdminShutdown             = "57P01"
	pgCodeCannotConnectNow          = "57P03"
)

func execLockRetry(timeout time.Duration, maxRetries int) dbx.ExecHookFunc {
	return func(q *dbx.Query, op func() error) error {
		if q.Context() == nil {
			cancelCtx, cancel := context.WithTimeout(context.Background(), timeout)
			defer func() {
				cancel()
				//nolint:staticcheck
				q.WithContext(nil) // reset
			}()
			q.WithContext(cancelCtx)
		}

		execErr := baseLockRetry(func(attempt int) error {
			return op()
		}, maxRetries)
		if execErr != nil && !errors.Is(execErr, sql.ErrNoRows) {
			execErr = fmt.Errorf("%w; failed query: %s", execErr, q.SQL())
		}

		return execErr
	}
}

func baseLockRetry(op func(attempt int) error, maxRetries int) error {
	attempt := 1

Retry:
	err := op(attempt)

	if err != nil && attempt <= maxRetries && isRetryableDBError(err) {
		time.Sleep(getDefaultRetryInterval(attempt))
		attempt++
		goto Retry
	}

	return err
}

// isRetryableDBError reports whether the given error is a transient
// database error that is safe to retry.
//
// For Postgres, this covers serialization failures (SQLSTATE 40001),
// deadlock-detected (40P01), and the class 08 / 57P connection-related
// transient errors.
func isRetryableDBError(err error) bool {
	if err == nil {
		return false
	}

	// structured Postgres error
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgCodeSerializationFailure,
			pgCodeDeadlockDetected,
			pgCodeConnectionException,
			pgCodeConnectionDoesNotExist,
			pgCodeConnectionFailure,
			pgCodeAdminShutdown,
			pgCodeCannotConnectNow:
			return true
		}
		return false
	}

	// fallback: some drivers / error wrappers surface the SQLSTATE or
	// the condition text only in the error message.
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 40001") ||
		strings.Contains(msg, "SQLSTATE 40P01") ||
		strings.Contains(msg, "could not serialize access") ||
		strings.Contains(msg, "deadlock detected")
}

func getDefaultRetryInterval(attempt int) time.Duration {
	if attempt < 0 || attempt > len(defaultRetryIntervals)-1 {
		return time.Duration(defaultRetryIntervals[len(defaultRetryIntervals)-1]) * time.Millisecond
	}

	return time.Duration(defaultRetryIntervals[attempt]) * time.Millisecond
}

