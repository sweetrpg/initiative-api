package relations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func server(t *testing.T, status int, body string, calls *atomic.Int32, gotAuth *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/authz/check-relation" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestAllowForwardsTheCallersBearer(t *testing.T) {
	var calls atomic.Int32
	var auth string
	c := server(t, 200, `{"allowed":true}`, &calls, &auth)
	ok, err := c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
	if err != nil || !ok {
		t.Fatalf("Check = %v, %v; want true, nil", ok, err)
	}
	if auth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want the caller's bearer", auth)
	}
}

func TestAllowIsCachedPerUserObjectAndRelation(t *testing.T) {
	var calls atomic.Int32
	c := server(t, 200, `{"allowed":true}`, &calls, nil)
	ctx := context.Background()
	for range 3 {
		_, _ = c.Check(ctx, "u1", "tok", "session:s1", "can_view")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 for repeated identical checks", calls.Load())
	}
	_, _ = c.Check(ctx, "u2", "tok2", "session:s1", "can_view")
	_, _ = c.Check(ctx, "u1", "tok", "session:s1", "can_edit")
	_, _ = c.Check(ctx, "u1", "tok", "session:s2", "can_view")
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4: another user, relation and object each miss the cache", calls.Load())
	}
}

func TestAllowExpires(t *testing.T) {
	var calls atomic.Int32
	c := server(t, 200, `{"allowed":true}`, &calls, nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	_, _ = c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
	now = now.Add(positiveTTL + time.Second)
	_, _ = c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 after the TTL", calls.Load())
	}
}

func TestDenyIsNotCached(t *testing.T) {
	var calls atomic.Int32
	c := server(t, 200, `{"allowed":false}`, &calls, nil)
	for range 2 {
		ok, err := c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
		if ok || err != nil {
			t.Fatalf("Check = %v, %v; want false, nil", ok, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2: a grant must show at once", calls.Load())
	}
}

func TestFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"server error", 500, ``, ErrUnavailable},
		{"authz unavailable", 503, `{"error":"authz_unavailable"}`, ErrUnavailable},
		{"bad body", 200, `not json`, ErrUnavailable},
		{"token rejected", 401, ``, ErrUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := server(t, tt.status, tt.body, &calls, nil)
			ok, err := c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
			if ok || !errors.Is(err, tt.want) {
				t.Fatalf("Check = %v, %v; want false, %v", ok, err, tt.want)
			}
		})
	}
}

func TestUnreachableFailsClosedAndKeepsNoStaleAllow(t *testing.T) {
	var calls atomic.Int32
	c := server(t, 200, `{"allowed":true}`, &calls, nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	_, _ = c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
	now = now.Add(time.Minute)
	c.base = "http://127.0.0.1:1"
	ok, err := c.Check(context.Background(), "u1", "tok", "session:s1", "can_view")
	if ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Check = %v, %v; want false, ErrUnavailable", ok, err)
	}
}

func TestMissingInputsAreRefused(t *testing.T) {
	var calls atomic.Int32
	c := server(t, 200, `{"allowed":true}`, &calls, nil)
	for _, a := range [][4]string{{"", "t", "o", "r"}, {"u", "", "o", "r"}, {"u", "t", "", "r"}, {"u", "t", "o", ""}} {
		if ok, err := c.Check(context.Background(), a[0], a[1], a[2], a[3]); ok || err == nil {
			t.Fatalf("Check%v = %v, %v; want refusal", a, ok, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("a refused check must not call auth-api")
	}
}
