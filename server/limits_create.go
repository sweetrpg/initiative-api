package server

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// requireRoom refuses a create that would exceed the caller's entitled count for key. Inactive
// items count toward the total owned, and an entitlements outage fails closed (503), while edits
// of existing items never come here (initiative-entitlement-gating, "Limit reached", "Creating
// while over the limit", "Service unavailable behavior"). It writes the error and returns false.
func requireRoom(c *gin.Context, ent *entitlements.Client, key string, owned func(ctx context.Context, owner string) (int64, error)) bool {
	owner := authz.Viewer(c)
	res, err := checkerFor(c, ent).Check(c.Request.Context(), key, entitlements.FailClosed)
	if err != nil {
		writeError(c, err, nil)
		return false
	}
	n, err := owned(c.Request.Context(), owner)
	if err != nil {
		writeError(c, err, nil)
		return false
	}
	if n >= int64(res.Limit) {
		writeError(c, &domain.EntitlementError{Key: key, Limit: res.Limit}, nil)
		return false
	}
	return true
}
