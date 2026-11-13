package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/sweetrpg/api-core.go/ratelimit"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

type fiveLimit struct{}

func (fiveLimit) Check(_ context.Context, key string, _ entitlements.Policy) (entitlements.Result, error) {
	return entitlements.Result{Key: key, Type: "limit", Limit: 5}, nil
}

// limitedRouter has a limiter with the given standard budget over a miniredis store, and a
// credential store; it returns two live credential secrets.
func limitedRouter(t *testing.T, limit int) (r *gin.Engine, mr *miniredis.Miniredis, secrets [2]string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mr = miniredis.RunT(t)
	addr := mr.Addr() // read before a test closes the server
	pool := &redis.Pool{Dial: func() (redis.Conn, error) { return redis.Dial("tcp", addr) }}
	t.Cleanup(func() { _ = pool.Close() })
	creds := domain.NewCredentials(fake.NewDB())
	if ok, err := creds.Open(context.Background()); err != nil || !ok {
		t.Fatalf("open = %v, %v", ok, err)
	}
	for i := range secrets {
		_, s, err := creds.Create(context.Background(), "u1", "foundry", []string{"read"}, fiveLimit{})
		if err != nil {
			t.Fatal(err)
		}
		secrets[i] = s
	}
	r = gin.New()
	opts := ratelimit.Options{Standard: ratelimit.Tier{Limit: limit, Window: 60}, Cheap: ratelimit.Tier{Limit: 100, Window: 60}}
	r.Use(RateLimit(pool, opts, creds)...)
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/status/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, mr, secrets
}

func hit(r http.Handler, bearer, apiKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCredentialOverItsLimitGets429WithoutAffectingOthers(t *testing.T) {
	r, _, s := limitedRouter(t, 2)
	hit(r, s[0], "")
	hit(r, s[0], "")

	over := hit(r, s[0], "")

	if over.Code != http.StatusTooManyRequests || over.Header().Get("Retry-After") == "" {
		t.Fatalf("third call = %d retry-after %q, want 429 with Retry-After", over.Code, over.Header().Get("Retry-After"))
	}
	if w := hit(r, s[1], ""); w.Code != http.StatusOK {
		t.Errorf("another credential from the same IP = %d, want 200", w.Code)
	}
	if w := hit(r, "user-token", ""); w.Code != http.StatusOK {
		t.Errorf("the owner's browser from the same IP = %d, want 200", w.Code)
	}
}

func TestSpoofedAPIKeyDoesNotGiveAFreshBucket(t *testing.T) {
	for _, who := range []string{"anonymous", "user", "credential"} {
		t.Run(who, func(t *testing.T) {
			r, _, s := limitedRouter(t, 2)
			bearer := map[string]string{"anonymous": "", "user": "user-token", "credential": s[0]}[who]
			var last int
			for i := range 4 {
				last = hit(r, bearer, "fresh-key-"+string(rune('a'+i))).Code
			}
			if last != http.StatusTooManyRequests {
				t.Fatalf("rotating X-API-Key: last = %d, want 429", last)
			}
		})
	}
}

func TestMadeUpCredentialSecretsShareTheIPBucket(t *testing.T) {
	r, _, _ := limitedRouter(t, 2)
	var last int
	for i := range 4 {
		last = hit(r, "ic_random-"+string(rune('a'+i)), "").Code
	}

	if last != http.StatusTooManyRequests {
		t.Fatalf("four made-up secrets from one IP: last = %d, want 429", last)
	}
}

func TestCounterStoreDownFailsClosedExceptStatus(t *testing.T) {
	r, mr, _ := limitedRouter(t, 5)
	mr.Close()

	if w := hit(r, "user-token", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("api route with store down = %d, want 503", w.Code)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status/ping", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status route with store down = %d, want 200", w.Code)
	}
}
