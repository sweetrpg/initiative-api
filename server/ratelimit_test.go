package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/sweetrpg/api-core.go/ratelimit"
)

func limitedRouter(t *testing.T, limit int) (*gin.Engine, *miniredis.Miniredis) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	addr := mr.Addr() // read before a test closes the server
	pool := &redis.Pool{Dial: func() (redis.Conn, error) { return redis.Dial("tcp", addr) }}
	t.Cleanup(func() { _ = pool.Close() })
	r := gin.New()
	opts := ratelimit.Options{Standard: ratelimit.Tier{Limit: limit, Window: 60}, Cheap: ratelimit.Tier{Limit: 100, Window: 60}}
	r.Use(RateLimit(pool, opts)...)
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/status/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, mr
}

func hit(r http.Handler, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCredentialOverItsLimitGets429WithoutAffectingOthers(t *testing.T) {
	r, _ := limitedRouter(t, 2)
	hit(r, "ic_one")
	hit(r, "ic_one")

	over := hit(r, "ic_one")

	if over.Code != http.StatusTooManyRequests || over.Header().Get("Retry-After") == "" {
		t.Fatalf("third call = %d retry-after %q, want 429 with Retry-After", over.Code, over.Header().Get("Retry-After"))
	}
	if w := hit(r, "ic_two"); w.Code != http.StatusOK {
		t.Errorf("another credential from the same IP = %d, want 200", w.Code)
	}
	if w := hit(r, "user-token"); w.Code != http.StatusOK {
		t.Errorf("the owner's browser from the same IP = %d, want 200", w.Code)
	}
}

func TestCallerCannotChooseItsCredentialBucket(t *testing.T) {
	r, _ := limitedRouter(t, 1)
	hit(r, "ic_one")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer ic_one")
	req.Header.Set("X-API-Key", "fresh-key-each-time")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("with a spoofed X-API-Key = %d, want 429", w.Code)
	}
}

func TestCounterStoreDownFailsClosedExceptStatus(t *testing.T) {
	r, mr := limitedRouter(t, 5)
	mr.Close()

	if w := hit(r, "user-token"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("api route with store down = %d, want 503", w.Code)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status/ping", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status route with store down = %d, want 200", w.Code)
	}
}
