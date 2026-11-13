// Package server wires the HTTP routes and middleware.
package server

import (
	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/constants"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/relations"
)

// Services are the dependencies the resource handlers need.
type Services struct {
	Authz        *authz.Client
	Entitlements *entitlements.Client
	Themes       *domain.Themes
	Credentials  *domain.Credentials
	Limits       *domain.Limits
	Locks        *domain.Locks
	Rosters      *domain.Rosters
	Encounters   *domain.Encounters
	Sessions     *domain.Sessions
	// Relations answers session authorization checks (PADR-0035). Without it every session
	// request fails closed with 503.
	Relations *relations.Client
}

// ProtectedGroup returns a route group whose every route requires a bearer token that auth-api
// verifies (signature, expiry, iss, aud) for this service and whose user holds one of roles
// (default: RoleUser). Missing or invalid tokens get 401, a missing role gets 403. Routes added
// to the group inherit this; handlers never repeat the check. The caller's own token is the only
// credential, per PADR-0037 (no service tokens), and a client application identity alone is a 401.
func ProtectedGroup(r *gin.Engine, client *authz.Client, creds *domain.Credentials, roles ...string) *gin.RouterGroup {
	if len(roles) == 0 {
		roles = []string{authz.RoleUser}
	}
	return r.Group("", typedErrors(), authenticate(creds, authz.RequireAnyRole(client, constants.ServiceName, roles...)))
}

// SetupHandlers registers every route group on r.
func SetupHandlers(r *gin.Engine, s Services) {
	setupStatusHandlers(r)

	g := ProtectedGroup(r, s.Authz, s.Credentials)
	g.Use(withChecker(s.Entitlements), withGate(s.Relations))
	if s.Limits != nil || s.Locks != nil {
		g.Use(reconcileLimits(s))
	}
	u := g.Group("", userOnly)
	setupThemeHandlers(u, s.Themes, s.Entitlements)
	setupEntitlementHandlers(u, s)
	setupCredentialHandlers(u, s)
	setupActiveItemHandlers(u, s)
	if s.Sessions != nil {
		setupSessionHandlers(u, s.Sessions)
	}
	setupRefHandlers(g, s.Encounters)
	if s.Credentials != nil {
		setupAdminCredentialHandlers(ProtectedGroup(r, s.Authz, nil, authz.RoleAdmin), s.Credentials)
	}
}

// withGate gives the data layer the relation checker and the caller's own bearer token (PADR-0037),
// so session data can be authorized per request. A nil client leaves ctx without a gate, which
// makes session data fail closed.
func withGate(c *relations.Client) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if c != nil {
			ctx.Request = ctx.Request.WithContext(data.WithGate(ctx.Request.Context(), c, authz.Token(ctx)))
		}
		ctx.Next()
	}
}
