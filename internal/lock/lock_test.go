package lock_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

func TestAcquire(t *testing.T) {
	t.Parallel()
	type want struct {
		busy    bool
		minWait time.Duration
	}
	tcs := []struct {
		name string
		held bool
		wait time.Duration
		want want
	}{
		{name: "free lock", held: false, wait: 0, want: want{busy: false}},
		{name: "held lock with no wait", held: true, wait: 0, want: want{busy: true}},
		{name: "held lock waits then gives up", held: true, wait: 50 * time.Millisecond, want: want{busy: true, minWait: 50 * time.Millisecond}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "x.lock")
			if tc.held {
				unlock, err := lock.Acquire(ctx, path, 0)
				require.NoError(t, err)
				defer unlock()
			}
			start := time.Now()
			unlock, err := lock.Acquire(ctx, path, tc.wait)
			if err == nil {
				unlock()
			}
			assert.Equal(t, tc.want.busy, err != nil)
			assert.GreaterOrEqual(t, time.Since(start), tc.want.minWait)
		})
	}
}

func TestReleaseLetsNextIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lock.Acquire(ctx, path, 0)
	require.NoError(t, err)
	unlock()
	again, err := lock.Acquire(ctx, path, 0)
	require.NoError(t, err)
	again()
}
