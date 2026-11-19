package store

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
)

// Reads are retried when a new connection to the server fails to open: pool growth or a replaced
// connection does a fresh handshake mid-run, and Atlas has been seen to refuse one with a SCRAM
// "bad auth" that the next attempt passes. A read changes nothing, so repeating it is safe; writes
// are not retried here (the driver's own retryable writes cover network errors).
const (
	readAttempts = 3
	readBackoff  = 100 * time.Millisecond
)

// readSleep waits between attempts; tests replace it.
var readSleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// connectErrorClass names the kind of connection failure err is, or "" when it is not one that a
// retry can fix.
func connectErrorClass(err error) string {
	if err == nil {
		return ""
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "auth error") || strings.Contains(m, "bad auth") || strings.Contains(m, "authenticationfailed"):
		return "handshake_auth"
	case strings.Contains(m, "connection handshake"):
		return "handshake"
	case mongo.IsNetworkError(err):
		return "network"
	}
	return ""
}

// retryRead runs op, repeating it up to readAttempts times while it fails with a connection
// failure, and logs the class of each failure it absorbs.
func retryRead[T any](ctx context.Context, name string, op func() (T, error)) (T, error) {
	backoff := readBackoff
	for attempt := 1; ; attempt++ {
		v, err := op()
		class := connectErrorClass(err)
		if class == "" || attempt >= readAttempts || ctx.Err() != nil {
			return v, err
		}
		slog.WarnContext(ctx, "database read failed to connect; retrying", "op", name, "class", class, "attempt", attempt, "error", err.Error())
		readSleep(ctx, backoff)
		backoff *= 2
	}
}
