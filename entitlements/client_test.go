package entitlements

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func stub(t *testing.T, status int, body string) (*Client, *atomic.Int32, *string) {
	t.Helper()
	var calls atomic.Int32
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth = r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), &calls, &auth
}

const limitBody = `{"key":"initiative.theme_slots","type":"limit","value":5,"ttl_seconds":60}`

func TestCheckForwardsUserTokenAndParsesLimit(t *testing.T) {
	c, _, auth := stub(t, 200, limitBody)
	res, err := c.Check(context.Background(), "u1", "tok", "initiative.theme_slots", FailClosed)
	if err != nil || res.Limit != 5 || res.Type != "limit" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if *auth != "Bearer tok" {
		t.Fatalf("Authorization = %q", *auth)
	}
}

func TestCheckParsesFlag(t *testing.T) {
	c, _, _ := stub(t, 200, `{"key":"k","type":"flag","value":true,"ttl_seconds":10}`)
	res, err := c.Check(context.Background(), "u1", "tok", "k", FailClosed)
	if err != nil || !res.Flag {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestCacheHonorsTTLPerUser(t *testing.T) {
	c, calls, _ := stub(t, 200, limitBody)
	now := time.Now()
	c.now = func() time.Time { return now }
	ctx := context.Background()

	_, _ = c.Check(ctx, "u1", "tok", "k", FailClosed)
	_, _ = c.Check(ctx, "u1", "tok", "k", FailClosed)
	if calls.Load() != 1 {
		t.Fatalf("second call within TTL hit the service: %d calls", calls.Load())
	}
	_, _ = c.Check(ctx, "u2", "tok2", "k", FailClosed)
	if calls.Load() != 2 {
		t.Fatalf("a different user must not share a cache entry: %d calls", calls.Load())
	}
	now = now.Add(61 * time.Second)
	_, _ = c.Check(ctx, "u1", "tok", "k", FailClosed)
	if calls.Load() != 3 {
		t.Fatalf("expired entry was not refetched: %d calls", calls.Load())
	}
}

func TestTTLIsCapped(t *testing.T) {
	c, _, _ := stub(t, 200, `{"key":"k","type":"limit","value":1,"ttl_seconds":86400}`)
	now := time.Now()
	c.now = func() time.Time { return now }
	_, _ = c.Check(context.Background(), "u1", "t", "k", FailClosed)
	if e := c.cache["u1\x00k"]; e.expires.Sub(now) != maxTTL {
		t.Fatalf("ttl = %v, want %v", e.expires.Sub(now), maxTTL)
	}
}

func TestUnreachableFailClosed(t *testing.T) {
	c, _, _ := stub(t, 503, "")
	_, err := c.Check(context.Background(), "u1", "t", "k", FailClosed)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestUnreachableFailOpenWithoutCacheIsDegraded(t *testing.T) {
	c, _, _ := stub(t, 500, "")
	res, err := c.Check(context.Background(), "u1", "t", "k", FailOpen)
	if err != nil || !res.Degraded {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestUnreachableFailOpenServesStale(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(limitBody))
	}))
	defer srv.Close()
	c := New(srv.URL)
	now := time.Now()
	c.now = func() time.Time { return now }
	_, _ = c.Check(context.Background(), "u1", "t", "k", FailClosed)

	down.Store(true)
	now = now.Add(time.Hour)
	res, err := c.Check(context.Background(), "u1", "t", "k", FailOpen)
	if err != nil || res.Degraded || res.Limit != 5 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, err := c.Check(context.Background(), "u1", "t", "k", FailClosed); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fail-closed must not use stale values: %v", err)
	}
}

func TestTransportErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := New(srv.URL).Check(context.Background(), "u1", "t", "k", FailClosed)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[int]error{401: ErrUnauthorized, 404: ErrUnknownKey} {
		c, _, _ := stub(t, status, "")
		// Neither is an outage, so FailOpen must still surface it.
		if _, err := c.Check(context.Background(), "u1", "t", "k", FailOpen); !errors.Is(err, want) {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
	}
}

func TestMalformedBodyIsUnavailable(t *testing.T) {
	for _, body := range []string{`not json`, `{"value":"x"}`, `{"value":-1}`, `{"value":1.5}`} {
		c, _, _ := stub(t, 200, body)
		if _, err := c.Check(context.Background(), "u1", "t", "k", FailClosed); !errors.Is(err, ErrUnavailable) {
			t.Errorf("body %q: err = %v", body, err)
		}
	}
}

func TestZeroTTLIsNotCached(t *testing.T) {
	c, calls, _ := stub(t, 200, `{"key":"k","type":"limit","value":1,"ttl_seconds":0}`)
	_, _ = c.Check(context.Background(), "u1", "t", "k", FailClosed)
	_, _ = c.Check(context.Background(), "u1", "t", "k", FailClosed)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestFailOpenStaleIsBounded(t *testing.T) {
	c, _, _ := stub(t, 200, limitBody)
	now := time.Now()
	c.now = func() time.Time { return now }
	_, _ = c.Check(context.Background(), "u1", "t", "initiative.theme_slots", FailClosed)
	c.http.Transport = roundTripFail{}

	now = now.Add(maxStale + 2*time.Minute)
	res, err := c.Check(context.Background(), "u1", "t", "initiative.theme_slots", FailOpen)
	if err != nil || !res.Degraded {
		t.Fatalf("got %+v, %v; want Degraded past maxStale", res, err)
	}
}

type roundTripFail struct{}

func (roundTripFail) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("down") }
