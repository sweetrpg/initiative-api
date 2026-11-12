package server

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
)

// reconcileEvery bounds how often one owner's limits are re-read on ordinary requests. The
// entitlements client caches each key for its own TTL, so a changed limit applies within the
// larger of the two.
var reconcileEvery = 30 * time.Second

// reconcileLimits applies the owner's current limits (inactive items beyond a reduced limit,
// reactivation on a raise) on requests carrying the owner's own bearer: that bearer is the only
// way to read their entitlements (PADR-0037), so integration credentials are skipped. A delete or
// a revoke that succeeded reconciles at once, because it freed a place. Failures never fail the
// request.
func reconcileLimits(s Services) gin.HandlerFunc {
	var last sync.Map // owner -> time of the last reconcile
	run := func(c *gin.Context, owner string) {
		last.Store(owner, time.Now())
		if err := s.Limits.Reconcile(c.Request.Context(), owner, checkerFor(c, s.Entitlements)); err != nil {
			slog.WarnContext(c.Request.Context(), "limit reconcile failed", "error", err)
		}
	}
	return func(c *gin.Context) {
		// The handler (and the authenticate middleware before it) set the viewer, so read it first.
		owner := authz.Viewer(c)
		_, viaCredential := c.Get(credentialContextKey)
		if owner == "" || viaCredential {
			c.Next()
			return
		}
		// Before the handler, so the response already reflects the current limits.
		if prev, ok := last.Load(owner); !ok || time.Since(prev.(time.Time)) >= reconcileEvery {
			run(c, owner)
		}
		c.Next()
		// A delete or revoke freed a place; an inactive item takes it now.
		if c.Writer.Status() < http.StatusMultipleChoices &&
			(c.Request.Method == http.MethodDelete || strings.HasSuffix(c.FullPath(), "/revoke")) {
			run(c, owner)
		}
	}
}

type activeItemBody struct {
	IDs []string `json:"ids"`
}

func setupActiveItemHandlers(g *gin.RouterGroup, s Services) {
	if s.Limits == nil {
		return
	}
	g.PUT("/entitlements/active/:kind", func(c *gin.Context) {
		var b activeItemBody
		if err := c.ShouldBindJSON(&b); err != nil || b.IDs == nil {
			badRequest(c, "request body must be JSON with an ids array")
			return
		}
		active, err := s.Limits.Choose(c.Request.Context(), authz.Viewer(c), c.Param("kind"), b.IDs, checkerFor(c, s.Entitlements))
		if err != nil {
			writeError(c, err, nil)
			return
		}
		c.JSON(http.StatusOK, gin.H{"kind": c.Param("kind"), "active": active})
	})
}
