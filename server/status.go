package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	apicores "github.com/sweetrpg/api-core.go/server"
	"github.com/sweetrpg/api-core.go/vo"
)

// Ping is the shallow liveness and readiness probe target (PADR-0017).
//
//	@Summary	Ping
//	@Tags		status
//	@Produce	json
//	@Success	200	{object}	vo.PingResponseVO
//	@Router		/status/ping [get]
func ping(c *gin.Context) {
	c.JSON(http.StatusOK, apicores.PingHandler())
}

// Readiness is the deep dependency check behind /status/health. PADR-0017 keeps it off the
// readiness and liveness probes (both use /status/ping): it is for operators and dashboards and
// must not gate routing or restarts. Each check runs under a timeout, and the answer is cached for ttl, so probes from every kubelet
// and load balancer cost the dependency one call per ttl, not one each.
type Readiness struct {
	checks  []Check
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu       sync.Mutex
	at       time.Time
	messages []string
}

// Check is one dependency's liveness test.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// NewReadiness builds the check set; a zero ttl or timeout takes 5s and 2s.
func NewReadiness(ttl, timeout time.Duration, checks ...Check) *Readiness {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Readiness{checks: checks, ttl: ttl, timeout: timeout, now: time.Now}
}

// Problems returns one message per failing check (none when ready), from the cache when it is
// younger than the ttl. It holds a lock while it checks, so concurrent probes share one round.
func (r *Readiness) Problems(ctx context.Context) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && r.now().Sub(r.at) < r.ttl {
		return r.messages
	}
	msgs := []string{}
	// The answer is cached for every caller, so a probe that hangs up must not cancel the check
	// and leave a false "unavailable" for the next ttl.
	base := context.WithoutCancel(ctx)
	for _, ch := range r.checks {
		cctx, cancel := context.WithTimeout(base, r.timeout)
		if err := ch.Fn(cctx); err != nil {
			slog.ErrorContext(ctx, "health check failed", "check", ch.Name, "error", err)
			msgs = append(msgs, ch.Name+": unavailable") // the detail stays in the log, not the response
		}
		cancel()
	}
	r.at, r.messages = r.now(), msgs
	return msgs
}

// Health is the deep dependency check. It answers 503 while a dependency is down. Probes use
// /status/ping (PADR-0017), so this never removes a pod from the Service or restarts it.
//
//	@Summary	Health check
//	@Tags		status
//	@Produce	json
//	@Success	200	{object}	vo.HealthResponseVO
//	@Failure	503	{object}	vo.HealthResponseVO
//	@Router		/status/health [get]
func health(r *Readiness) gin.HandlerFunc {
	return func(c *gin.Context) {
		msgs := []string{}
		if r != nil {
			msgs = r.Problems(c.Request.Context())
		}
		status := http.StatusOK
		if len(msgs) > 0 {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, vo.HealthResponseVO{Messages: msgs, Errors: len(msgs)})
	}
}

func setupStatusHandlers(r *gin.Engine, ready *Readiness) {
	r.GET("/status/ping", ping)
	r.GET("/status/health", health(ready))
}
