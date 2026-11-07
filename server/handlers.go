// Package server wires the HTTP routes and middleware.
package server

import (
	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/constants"
)

// ProtectedGroup returns a route group whose every route requires a bearer token that auth-api
// verifies (signature, expiry, iss, aud) for this service and whose user holds one of roles
// (default: RoleUser). Missing or invalid tokens get 401, a missing role gets 403. Routes added
// to the group inherit this; handlers never repeat the check. The caller's own token is the only
// credential, per PADR-0037 (no service tokens), and a client application identity alone is a 401.
func ProtectedGroup(r *gin.Engine, client *authz.Client, roles ...string) *gin.RouterGroup {
	if len(roles) == 0 {
		roles = []string{authz.RoleUser}
	}
	return r.Group("", authz.RequireAnyRole(client, constants.ServiceName, roles...))
}

// SetupHandlers registers every route group on r.
func SetupHandlers(r *gin.Engine, client *authz.Client) {
	setupStatusHandlers(r)

	// Resource handlers register on this group (tasks 2.x and 3.x).
	_ = ProtectedGroup(r, client)
}
