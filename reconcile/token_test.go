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
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", f.calls), "token_type": "Bearer", "expires_in": f.expiresIn})
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

func TestASecond401AfterARefreshIsATokenRejection(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	if err := mr.Set(tokenCacheKey, "stale"); err != nil {
		t.Fatal(err)
	}
	srv, requests := authAPIAccepting(t)

	_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	if !errors.Is(err, ErrTokenRejected) || *requests != 2 || f.calls != 1 || !mr.Exists(rejectedKey) {
		t.Fatalf("err=%v requests=%d auth0=%d marker=%v; want one refresh, one retry, rejection and a marker", err, *requests, f.calls, mr.Exists(rejectedKey))
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

func TestAPersistent401SpendsABoundedNumberOfAuth0TokensAcrossRuns(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	srv, _ := authAPIAccepting(t) // rejects every token

	run := func() error {
		_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")
		return err
	}
	err1 := run()
	callsAfterFirst := f.calls
	var later []error
	for i := 0; i < 5; i++ {
		later = append(later, run())
	}

	if !errors.Is(err1, ErrTokenRejected) || callsAfterFirst > 2 {
		t.Fatalf("first run err=%v auth0 calls=%d; want ErrTokenRejected with at most 2 calls", err1, callsAfterFirst)
	}
	for _, err := range later {
		if !errors.Is(err, ErrTokenRejected) {
			t.Errorf("later run err = %v, want ErrTokenRejected", err)
		}
	}
	if f.calls != callsAfterFirst {
		t.Errorf("auth0 calls = %d after later runs, want no more than %d while the marker lives", f.calls, callsAfterFirst)
	}
	if got := mr.TTL(rejectedKey); got <= 0 || got > rejectedTTL {
		t.Errorf("marker ttl = %v, want up to %v", got, rejectedTTL)
	}
}

func TestA401OnAFreshlyFetchedTokenIsNotRefreshedAgain(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	srv, requests := authAPIAccepting(t)

	_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	if !errors.Is(err, ErrTokenRejected) || f.calls != 1 || *requests != 1 {
		t.Fatalf("err=%v auth0=%d requests=%d; want one fetch, one request, rejection", err, f.calls, *requests)
	}
}

func TestASuccessfulCallClearsTheRejectionMarker(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	srv, _ := authAPIAccepting(t, "tok-1")
	if err := mr.Set(rejectedKey, "1"); err != nil {
		t.Fatal(err)
	}
	mr.Del(rejectedKey) // an operator deleted it after fixing the cause

	_, err := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	if err != nil || mr.Exists(rejectedKey) {
		t.Fatalf("err=%v marker=%v; want success and no marker", err, mr.Exists(rejectedKey))
	}
}

func TestAMarkerFromEarlierBlocksTheRunUntilItIsClearedThenSuccessRemovesIt(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	srv, _ := authAPIAccepting(t, "tok-1")
	if err := mr.Set(rejectedKey, "1"); err != nil {
		t.Fatal(err)
	}

	_, blocked := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")
	mr.Del(rejectedKey)
	_, ok := NewAuthAPIWithSource(srv.URL, newSource(t, f, mr)).Read(context.Background(), "session:s1")

	if !errors.Is(blocked, ErrTokenRejected) || ok != nil || f.calls != 1 {
		t.Fatalf("blocked=%v ok=%v auth0=%d; want blocked without an Auth0 call, then success", blocked, ok, f.calls)
	}
}

func TestTheTokenClientDoesNotFollowRedirectsWithTheSecret(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	mr := miniredis.RunT(t)
	src := newSource(t, newFakeAuth0(t), mr)
	src.TokenURL = redirect.URL

	_, err := src.Token(context.Background())

	if err == nil || leaked || !strings.Contains(err.Error(), "307") {
		t.Fatalf("err=%v leaked=%v; want a 307 failure and no request to the redirect target", err, leaked)
	}
}

func TestAHugeExpiresInIsCappedAtThirtyDays(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	f.expiresIn = 1 << 60

	if _, err := newSource(t, f, mr).Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := mr.TTL(tokenCacheKey); got > maxLifetime || got <= 0 {
		t.Fatalf("cache ttl = %v, want at most %v", got, maxLifetime)
	}
}

func TestATokenThatIsNotABearerTokenIsRefused(t *testing.T) {
	mr := miniredis.RunT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "token_type": "mac", "expires_in": 600})
	}))
	defer srv.Close()
	f := &fakeAuth0{srv: srv}

	if _, err := newSource(t, f, mr).Token(context.Background()); err == nil {
		t.Fatal("err = nil, want a refusal")
	}
}

func TestAnUnreachableAuth0NamesTheFailureClassNotTheURL(t *testing.T) {
	f, mr := newFakeAuth0(t), miniredis.RunT(t)
	src := newSource(t, f, mr)
	f.srv.Close()

	_, err := src.Token(context.Background())

	if err == nil || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), f.srv.URL) {
		t.Fatalf("err = %v, want the class and no URL", err)
	}
}
