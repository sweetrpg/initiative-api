package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
)

const testSecret = "s3cret-value"

// fakeAuth0 is a token endpoint that issues tok-1, tok-2, ... and records what it was asked.
type fakeAuth0 struct {
	srv       *httptest.Server
	calls     int
	expiresIn int64
	status    int // when non-zero, every request gets this status
	lastBody  map[string]string
}

func newFakeAuth0(t *testing.T) *fakeAuth0 {
	t.Helper()
	f := &fakeAuth0{expiresIn: 86400}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"too_many_requests","echo":"` + testSecret + `"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", f.calls), "expires_in": f.expiresIn})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newSource(t *testing.T, f *fakeAuth0, mr *miniredis.Miniredis) *ClientCredentials {
	t.Helper()
	addr := mr.Addr() // read now: Addr panics once the server is closed
	pool := &redis.Pool{Dial: func() (redis.Conn, error) { return redis.Dial("tcp", addr) }}
	t.Cleanup(func() { _ = pool.Close() })
	return NewClientCredentials(f.srv.URL, "client-id", testSecret, "https://sweetrpg.com/api/", pool)
}

func TestFetchSendsTheClientCredentialsGrantAndCachesUntilTwoMinutesBeforeExpiry(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)

	tok, err := newSource(t, f, mr).Token(context.Background())

	if err != nil || tok != "tok-1" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
	if f.lastBody["grant_type"] != "client_credentials" || f.lastBody["client_id"] != "client-id" || f.lastBody["audience"] != "https://sweetrpg.com/api/" {
		t.Errorf("request = %v, want the client-credentials grant for the audience", f.lastBody)
	}
	if got, want := mr.TTL(tokenCacheKey), time.Duration(86400-120)*time.Second; got != want {
		t.Errorf("cache ttl = %v, want %v", got, want)
	}
}

func TestACachedTokenIsReusedWithoutCallingAuth0(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	if _, err := newSource(t, f, mr).Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	tok, err := newSource(t, f, mr).Token(context.Background())

	if err != nil || tok != "tok-1" || f.calls != 1 {
		t.Fatalf("token=%q err=%v calls=%d; want the cached token and one Auth0 call", tok, err, f.calls)
	}
}

func TestATokenWithTwoMinutesOrLessIsNotCached(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.expiresIn = 120

	tok, err := newSource(t, f, mr).Token(context.Background())

	if err != nil || tok != "tok-1" || mr.Exists(tokenCacheKey) {
		t.Fatalf("token=%q err=%v cached=%v; want it used but not cached", tok, err, mr.Exists(tokenCacheKey))
	}
}

func TestAnAuth0FailureFailsOnceWithoutTheSecretAndIsNotRetriedInTheRun(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.status = http.StatusTooManyRequests
	src := newSource(t, f, mr)

	_, err1 := src.Token(context.Background())
	_, err2 := src.Token(context.Background())

	if err1 == nil || err2 == nil || f.calls != 1 {
		t.Fatalf("err1=%v err2=%v calls=%d; want both to fail with one Auth0 call", err1, err2, f.calls)
	}
	if strings.Contains(err1.Error(), testSecret) || !strings.Contains(err1.Error(), "429") {
		t.Errorf("error = %q, want the status and no secret", err1)
	}
	if mr.Exists(tokenCacheKey) {
		t.Error("a failed fetch left a cache entry")
	}
}

func TestAnUnreachableAuth0IsAnErrorWithoutTheSecret(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	src := newSource(t, f, mr)
	f.srv.Close()

	_, err := src.Token(context.Background())

	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("err = %v, want a failure that does not contain the secret", err)
	}
}

func TestARedisFailureFailsTheRunRatherThanBurningAuth0Tokens(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	src := newSource(t, f, mr)
	mr.Close()

	_, err := src.Token(context.Background())

	if err == nil || f.calls != 0 {
		t.Fatalf("err=%v calls=%d; want an error and no Auth0 call", err, f.calls)
	}
}

// authAPIAccepting answers 200 only for the listed bearer tokens and counts requests.
func authAPIAccepting(t *testing.T, ok ...string) (*httptest.Server, *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		for _, tok := range ok {
			if r.Header.Get("Authorization") == "Bearer "+tok {
				_ = json.NewEncoder(w).Encode(map[string]any{"tuples": []Tuple{}})
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestA401ClearsTheCachedTokenFetchesOnceMoreAndRetries(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	if err := mr.Set(tokenCacheKey, "stale"); err != nil {
		t.Fatal(err)
	}
	srv, requests := authAPIAccepting(t, "tok-1")

	_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	got, _ := mr.Get(tokenCacheKey)
	if err != nil || *requests != 2 || f.calls != 1 || got != "tok-1" {
		t.Fatalf("err=%v requests=%d auth0=%d cached=%q; want a retry with a fresh token that replaces the stale one", err, *requests, f.calls, got)
	}
}

func TestASecond401IsATokenRejection(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	srv, requests := authAPIAccepting(t)

	_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	if !errors.Is(err, ErrTokenRejected) || *requests != 2 || f.calls != 2 {
		t.Fatalf("err=%v requests=%d auth0=%d; want ErrTokenRejected after one retry with one fresh token (2 Auth0 calls: initial and refresh)", err, *requests, f.calls)
	}
}

func TestA403IsNotRetried(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n++; w.WriteHeader(403) }))
	defer srv.Close()

	_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	if !errors.Is(err, ErrTokenRejected) || n != 1 {
		t.Fatalf("err=%v requests=%d; want ErrTokenRejected without a retry", err, n)
	}
}
