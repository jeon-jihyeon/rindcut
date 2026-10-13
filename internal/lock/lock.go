// Package lock serializes rindcut processes on one machine with advisory file locks
package lock

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const poll = 5 * time.Millisecond

// ErrBusy means another process held the lock past the wait
var ErrBusy = errors.New("held by another rindcut process")

// Acquire takes an exclusive lock on path, waiting up to wait
// A zero wait tries once, for a caller that should skip its work rather than queue behind another run
func Acquire(ctx context.Context, path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			_ = f.Close()
			return nil, ErrBusy
		}
		time.Sleep(poll)
	}
}
