package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHealthIs503WhileADependencyIsDownAndPingStaysUp(t *testing.T) {
	var down atomic.Bool
	var now atomic.Int64
	ready := NewReadiness(5*time.Second, time.Second, Check{Name: "database", Fn: func(context.Context) error {
		if down.Load() {
			return errors.New("server selection timeout")
		}
		return nil
	}})
	ready.now = func() time.Time { return time.Unix(0, now.Load()) }
	r := gin.New()
	setupStatusHandlers(r, ready)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/status/health"); w.Code != http.StatusOK {
		t.Fatalf("healthy = %d", w.Code)
	}
	down.Store(true)
	now.Add(int64(6 * time.Second)) // past the cache
	w := get("/status/health")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "database") {
		t.Errorf("database down = %d %s, want 503 naming it", w.Code, w.Body)
	}
	if p := get("/status/ping"); p.Code != http.StatusOK {
		t.Errorf("ping = %d, want liveness untouched by a dependency", p.Code)
	}
}

func TestReadinessChecksOncePerTTLAndTimesTheCheckOut(t *testing.T) {
	var calls atomic.Int32
	var now atomic.Int64
	ready := NewReadiness(5*time.Second, 50*time.Millisecond, Check{Name: "database", Fn: func(ctx context.Context) error {
		calls.Add(1)
		<-ctx.Done() // a hung dependency
		return ctx.Err()
	}})
	ready.now = func() time.Time { return time.Unix(0, now.Load()) }
	start := time.Now()
	for range 3 {
		if p := ready.Problems(context.Background()); len(p) != 1 {
			t.Fatalf("problems = %v", p)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d checks for three probes inside the ttl, want 1", calls.Load())
	}
	if time.Since(start) > time.Second {
		t.Error("a hung dependency held the probe past its timeout")
	}
	now.Add(int64(6 * time.Second))
	ready.Problems(context.Background())
	if calls.Load() != 2 {
		t.Errorf("%d checks after the ttl, want 2", calls.Load())
	}
}

func TestAServiceWithNoChecksIsHealthy(t *testing.T) {
	r := gin.New()
	setupStatusHandlers(r, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/status/health", nil))
	if w.Code != http.StatusOK {
		t.Errorf("= %d", w.Code)
	}
}
