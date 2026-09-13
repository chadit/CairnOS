package bridge

import (
	"context"
	"time"
)

const (
	// DefaultBackoffBase is the first retry delay a transport's connection loop
	// waits after a failure; each further failure doubles it up to BackoffCap.
	DefaultBackoffBase = 1 * time.Second

	// BackoffCap is the ceiling the doubling stops at, so a server that stays
	// down is retried once a minute rather than never again.
	BackoffCap = 1 * time.Minute
)

// Sleep waits for delay or for ctx to end, reporting false when ctx ended so the
// caller stops its loop instead of continuing. Every transport's reconnect loop
// needs the same cancellable wait: Telegram's long poll and Mattermost's event
// socket both fail the same way and retry on the same schedule.
func Sleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// NextBackoff doubles a retry delay up to BackoffCap.
func NextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > BackoffCap {
		return BackoffCap
	}

	return next
}
