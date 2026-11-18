package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errHandshake = errors.New(`connection() error occurred during connection handshake: auth error: sasl conversation error: unable to authenticate using mechanism "SCRAM-SHA-1": bad auth`)

func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := readSleep
	readSleep = func(_ context.Context, d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { readSleep = old })
	return &slept
}

func TestARead_FailingToConnectIsRetriedUntilItSucceeds(t *testing.T) {
	slept := noSleep(t)
	calls := 0
	v, err := retryRead(context.Background(), "t", func() (int, error) {
		if calls++; calls < 3 {
			return 0, errHandshake
		}
		return 7, nil
	})
	if err != nil || v != 7 || calls != 3 {
		t.Fatalf("v=%d err=%v calls=%d, want 7, nil, 3", v, err, calls)
	}
	if len(*slept) != 2 || (*slept)[1] != 2*(*slept)[0] {
		t.Errorf("backoff = %v, want two doubling waits", *slept)
	}
}

func TestAReadGivesUpAfterTheAttemptLimitAndReturnsTheError(t *testing.T) {
	noSleep(t)
	calls := 0
	_, err := retryRead(context.Background(), "t", func() (int, error) { calls++; return 0, errHandshake })
	if !errors.Is(err, errHandshake) || calls != readAttempts {
		t.Errorf("err=%v calls=%d, want the handshake error after %d tries", err, calls, readAttempts)
	}
}

func TestAnErrorThatIsNotAConnectionFailureIsNotRetried(t *testing.T) {
	noSleep(t)
	for _, e := range []error{ErrNotFound, errors.New("bad query"), context.DeadlineExceeded} {
		calls := 0
		_, err := retryRead(context.Background(), "t", func() (int, error) { calls++; return 0, e })
		if err != e || calls != 1 {
			t.Errorf("%v: err=%v calls=%d, want one try", e, err, calls)
		}
	}
}

func TestNoRetryOnceTheContextIsDone(t *testing.T) {
	noSleep(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, _ = retryRead(ctx, "t", func() (int, error) { calls++; return 0, errHandshake })
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestConnectErrorClasses(t *testing.T) {
	for msg, want := range map[string]string{
		"x: auth error: bad auth":                                      "handshake_auth",
		"connection() error occurred during connection handshake: EOF": "handshake",
		"some other failure":                                           "",
	} {
		if got := connectErrorClass(errors.New(msg)); got != want {
			t.Errorf("%q = %q, want %q", msg, got, want)
		}
	}
}
