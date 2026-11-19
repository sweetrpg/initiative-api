// Package server wires the HTTP routes and middleware.
package server

import (
	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/constants"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/relations"
)

// Services are the dependencies the resource handlers need.
type Services struct {
	// Ready is the deep dependency check behind /status/health; nil reports healthy.
	Ready        *Readiness
	Authz        *authz.Client
	Entitlements *entitlements.Client
	// OwnerLimits records the owner's entitlement values for integration credentials, which carry
	// no user bearer. Optional; without it a credential's limit checks fail closed.
	OwnerLimits *data.OwnerEntitlements
	Themes      *domain.Themes
	Credentials *domain.Credentials
	Limits      *domain.Limits
	Locks       *domain.Locks
	Rosters     *domain.Rosters
	Encounters  *domain.Encounters
	Sessions    *domain.Sessions
	// ChangeRequests handles player-submitted combatant changes; it needs Encounters and Sessions.
	ChangeRequests *domain.ChangeRequests
	Systems        *data.SystemRepo
	// Relations answers session authorization checks (PADR-0035). Without it every session
	// request fails closed with 503.
	Relations *relations.Client
	// Streams and Hub serve the event stream; without both there is no /streams route. Stream
	// holds its transport settings.
	Streams *domain.Streams
	Hub     *stream.Hub
	Stream  StreamOptions
	// Leases counts a user's open streams across pods; without it the per-user cap is not applied.
	Leases *data.StreamLeases
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
	setupStatusHandlers(r, s.Ready)

	g := ProtectedGroup(r, s.Authz, s.Credentials)
	g.Use(limitBody(), withOrigin(), withOwnerRecord(s.OwnerLimits), withChecker(s.Entitlements), withGate(s.Relations))
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
	if s.Encounters != nil && s.Sessions != nil {
		setupEncounterViewHandlers(u, s.Encounters, s.Sessions)
	}
	if s.ChangeRequests != nil {
		setupChangeRequestHandlers(u, s.ChangeRequests)
	}
	if s.Encounters != nil {
		setupEncounterHandlers(g, s.Encounters, s.Rosters, s.Entitlements)
	}
	if s.Rosters != nil {
		setupRosterHandlers(g, s.Rosters, s.Entitlements)
	}
	if s.Systems != nil {
		setupGameSystemHandlers(g, s.Systems, s.Entitlements)
	}
	setupRefHandlers(g, s.Encounters)
	if s.Streams != nil && s.Hub != nil {
		setupStreamHandlers(r, u, s)
	}
	if s.Credentials != nil {
		setupAdminCredentialHandlers(ProtectedGroup(r, s.Authz, nil, authz.RoleAdmin), s.Credentials)
	}
}

// withGate gives the data layer the relation checker, and the domain auth-api's session surface,
// both with the caller's own bearer token (PADR-0037), so session data can be authorized and
// membership tuples written per request. A nil client leaves ctx without a gate, which
// makes session data fail closed.
func withGate(c *relations.Client) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if c != nil {
			token := authz.Token(ctx)
			rc := data.WithGate(ctx.Request.Context(), c, token)
			ctx.Request = ctx.Request.WithContext(domain.WithAuthority(rc, c, token))
		}
		ctx.Next()
	}
}
