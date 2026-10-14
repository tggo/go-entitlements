package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pgErr(code string) error {
	return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
}

func TestWithRetry(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tests := []struct {
		name      string
		errs      []error // returned per attempt, then nil
		wantCalls int
		wantErr   error
	}{
		{"success first try", nil, 1, nil},
		{"serialization failure then success", []error{pgErr("40001")}, 2, nil},
		{"deadlock then success", []error{pgErr("40P01")}, 2, nil},
		{"other pg error is not retried", []error{pgErr("23505")}, 1, nil},
		{"plain error is not retried", []error{boom}, 1, boom},
		{"bounded", []error{pgErr("40001"), pgErr("40001"), pgErr("40001"), pgErr("40001"), pgErr("40001"), pgErr("40001")}, maxAttempts, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			err := withRetry(context.Background(), func() error {
				defer func() { calls++ }()
				if calls < len(tc.errs) {
					return tc.errs[calls]
				}
				return nil
			})
			assert.Equal(t, tc.wantCalls, calls)
			switch {
			case tc.wantErr != nil:
				assert.ErrorIs(t, err, tc.wantErr)
			case tc.name == "bounded" || tc.name == "other pg error is not retried":
				assert.Error(t, err)
			default:
				assert.NoError(t, err)
			}
		})
	}
}

func TestWithRetry_ContextCancelStopsBackoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := withRetry(ctx, func() error { calls++; return pgErr("40001") })
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}
