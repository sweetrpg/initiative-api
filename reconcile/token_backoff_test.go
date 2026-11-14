package reconcile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func failOnce(t *testing.T, f *fakeAuth0, mr *miniredis.Miniredis) error {
	t.Helper()
	_, err := newSource(t, f, mr).Token(context.Background())
	if err == nil {
		t.Fatal("token = ok, want a failure")
	}
	return err
}

func TestAFailedFetchArmsTheBackoffForFiveMinutesAndCountsTheFailure(t *testing.T) {
	cases := map[string]struct {
		status int
		retry  string
		want   time.Duration
	}{
		"429 without Retry-After":    {429, "", 5 * time.Minute},
		"429 with a longer one":      {429, "600", 10 * time.Minute},
		"429 with a shorter one":     {429, "30", 5 * time.Minute},
		"429 with a huge one":        {429, "999999", time.Hour},
		"429 with a non-number date": {429, "Wed, 21 Oct 2026 07:28:00 GMT", 5 * time.Minute},
		"500":                        {500, "", 5 * time.Minute},
		"401 from Auth0":             {401, "", 5 * time.Minute},
		"403 from Auth0":             {403, "", 5 * time.Minute},
		"Retry-After ignored on 500": {500, "900", 5 * time.Minute},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, mr := newFakeAuth0(t), miniredis.RunT(t)
			f.status, f.retry = tc.status, tc.retry

			_ = failOnce(t, f, mr)

			if got := mr.TTL(auth0BackoffKey); got != tc.want {
				t.Errorf("backoff ttl = %v, want %v", got, tc.want)
			}
			if n, _ := mr.Get(auth0FailuresKey); n != "1" {
				t.Errorf("failures = %q, want 1", n)
			}
		})
	}
}

func TestANetworkErrorAndAMalformed200AlsoArmTheBackoff(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.srv.Close()
	_ = failOnce(t, f, mr)
	if mr.TTL(auth0BackoffKey) != 5*time.Minute {
		t.Errorf("network error: backoff ttl = %v, want 5m", mr.TTL(auth0BackoffKey))
	}

	f2, mr2 := newFakeAuth0(t), miniredis.RunT(t)
	f2.expiresIn = 0 // a 200 without a usable lifetime
	_ = failOnce(t, f2, mr2)
	if mr2.TTL(auth0BackoffKey) != 5*time.Minute {
		t.Errorf("malformed 200: backoff ttl = %v, want 5m", mr2.TTL(auth0BackoffKey))
	}
}

func TestTheBackoffDoublesToAnHourAndStaysThere(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = 500
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour, time.Hour}

	for i, w := range want {
		_ = failOnce(t, f, mr)
		if got := mr.TTL(auth0BackoffKey); got != w {
			t.Fatalf("failure %d: backoff ttl = %v, want %v", i+1, got, w)
		}
		mr.FastForward(w + time.Second) // the backoff and the fetch guard expire; the count lives 2h
	}
}

func TestTheBackoffBlocksEverySourceWithoutCallingAuth0(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = 429
	_ = failOnce(t, f, mr)
	mr.FastForward(2 * time.Minute) // past the fetch guard, inside the backoff

	for i := 0; i < 3; i++ {
		_, err := newSource(t, f, mr).Token(context.Background())
		if !errors.Is(err, ErrAuth0Backoff) || errors.Is(err, ErrTokenRejected) {
			t.Fatalf("run %d: err = %v, want ErrAuth0Backoff and not a token rejection", i, err)
		}
	}

	if f.calls != 1 {
		t.Fatalf("auth0 calls = %d, want 1 (the first failure only)", f.calls)
	}
}

func TestACachedTokenIsStillServedDuringTheBackoff(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	if err := mr.Set(tokenCacheKey, "cached"); err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(auth0BackoffKey, "1"); err != nil {
		t.Fatal(err)
	}
	mr.SetTTL(auth0BackoffKey, time.Hour)

	tok, err := newSource(t, f, mr).Token(context.Background())

	if err != nil || tok != "cached" || f.calls != 0 {
		t.Fatalf("token=%q err=%v calls=%d; want the cached token and no Auth0 call", tok, err, f.calls)
	}
}

func TestTheProbeAfterTheBackoffClearsBothKeysOnSuccess(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = 500
	_ = failOnce(t, f, mr)
	mr.FastForward(5*time.Minute + time.Second)
	f.status = 0

	tok, err := newSource(t, f, mr).Token(context.Background())

	if err != nil || tok == "" || mr.Exists(auth0BackoffKey) || mr.Exists(auth0FailuresKey) {
		t.Fatalf("tok=%q err=%v backoff=%v failures=%v; want success and both keys cleared", tok, err, mr.Exists(auth0BackoffKey), mr.Exists(auth0FailuresKey))
	}
}

func TestAFailedProbeIncrementsAndReArms(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = 500
	_ = failOnce(t, f, mr)
	mr.FastForward(5*time.Minute + time.Second)

	_ = failOnce(t, f, mr)

	if got := mr.TTL(auth0BackoffKey); got != 10*time.Minute {
		t.Fatalf("backoff ttl = %v, want 10m after a failed probe", got)
	}
}

func TestTheFailureCountExpiresAfterTwoHoursAndRestartsAtFiveMinutes(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = 500
	_ = failOnce(t, f, mr)
	if got := mr.TTL(auth0FailuresKey); got != failuresTTL {
		t.Fatalf("failures ttl = %v, want %v", got, failuresTTL)
	}
	mr.FastForward(failuresTTL + time.Second)

	_ = failOnce(t, f, mr)

	if got, n := mr.TTL(auth0BackoffKey), func() string { v, _ := mr.Get(auth0FailuresKey); return v }(); got != 5*time.Minute || n != "1" {
		t.Fatalf("backoff ttl=%v failures=%q; want 5m and a count restarted at 1", got, n)
	}
}

func TestAFailedBackoffWriteIsReturnedWithTheFetchError(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = 500

	_, err := sourceWithBrokenSet(t, f, mr, auth0BackoffKey).Token(context.Background())

	if err == nil || !strings.Contains(err.Error(), "arm auth0 backoff") || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v, want the fetch error joined with the failed backoff write", err)
	}
}

func TestBackoffLogsAndErrorsNeverContainTheSecretOrTheResponseBody(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = http.StatusTooManyRequests // its body echoes the secret

	err1 := failOnce(t, f, mr)
	mr.FastForward(2 * time.Minute)
	_, err2 := newSource(t, f, mr).Token(context.Background())

	for _, text := range []string{logs.String(), err1.Error(), err2.Error()} {
		if strings.Contains(text, testSecret) || strings.Contains(text, "too_many_requests") {
			t.Fatalf("leaked the secret or response body: %q", text)
		}
	}
	if !strings.Contains(logs.String(), "backing off") {
		t.Errorf("logs = %q, want a backoff line", logs.String())
	}
}
